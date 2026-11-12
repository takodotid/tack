//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCredentialsSignalHelper is the tack that TestSignalEndsWaitForCredentials
// sends SIGTERM to: tack status, whose site a has a credential plugin that
// waits.
func TestCredentialsSignalHelper(t *testing.T) {
	if os.Getenv("TACK_CREDENTIALS_HELPER") == "" {
		t.Skip("run by TestSignalEndsWaitForCredentials")
	}
	os.Args = []string{"tack", "status", "-f", os.Getenv("TACK_CREDENTIALS_WORKLOAD"), "--kubeconfig", os.Getenv("TACK_CREDENTIALS_KUBECONFIG")}
	main()
}

// Found in the coordinator's review of PR #68: a SIGTERM to tack's pid
// alone left tack waiting for a credential plugin, which client-go runs
// with no context. The signal now ends the wait, and tack exits while the
// plugin still runs ([RFC#0007] Requirement 12).
func TestSignalEndsWaitForCredentials(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "plugin.log")
	plugin := filepath.Join(dir, "plugin.sh")
	script := fmt.Sprintf("#!/bin/sh\necho \"start $$\" >>%q\nexec sleep 20\n", log)
	if err := os.WriteFile(plugin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(dir, "kubeconfig")
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://127.0.0.1:1"}
users:
- name: a
  user: {exec: {apiVersion: client.authentication.k8s.io/v1, command: %q, interactiveMode: Never}}
contexts:
- name: site-a
  context: {cluster: c, user: a}
- name: site-b
  context: {cluster: c, user: a}
`, plugin)
	if err := os.WriteFile(kubeconfig, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	workload, err := filepath.Abs("../../internal/cli/testdata/workload-unmapped.yaml")
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestCredentialsSignalHelper$")
	cmd.Env = append(os.Environ(), "TACK_CREDENTIALS_HELPER=1",
		"TACK_CREDENTIALS_WORKLOAD="+workload, "TACK_CREDENTIALS_KUBECONFIG="+kubeconfig)
	// A file, not a pipe: the plugin inherits tack's stderr, and a pipe
	// would stay open, and Wait wait, until the plugin ends.
	outFile, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	cmd.Stdout, cmd.Stderr = outFile, outFile
	output := func() string { b, _ := os.ReadFile(outFile.Name()); return string(b) }
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		// The plugin outlives tack: end it, as the operator's terminal
		// would with Ctrl-C.
		if pid := pluginPid(log); pid != 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for b, _ := os.ReadFile(log); !strings.Contains(string(b), "start"); b, _ = os.ReadFile(log) {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("the credential plugin did not start:\n%s", output())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("tack exited 0 after SIGTERM:\n%s", output())
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatalf("tack still waited for the credential plugin 5 s after SIGTERM:\n%s", output())
	}
	if pid := pluginPid(log); pid == 0 || syscall.Kill(pid, 0) != nil {
		t.Errorf("the plugin ended before tack: the test proves nothing")
	}
}

// pluginPid returns the pid that the plugin logged, or 0.
func pluginPid(log string) int {
	b, _ := os.ReadFile(log)
	pid, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(string(b), "start ")))
	return pid
}
