package gitops

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/stop"
)

// stalled returns the URL of an HTTP remote that accepts connections and
// never answers, as a remote that drops packets after the connect does.
func stalled(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { <-done; c.Close() }()
		}
	}()
	return "http://" + l.Addr().String() + "/gitops.git"
}

// #35 row 12 (RFC0006 Requirement 3). A fetch from a remote that never
// answers ends at the command's deadline, and counts as unreachable: the
// Git step waits with GitUnreachable instead of hanging.
func TestFetchFromStalledRemoteIsUnreachable(t *testing.T) {
	isolate(t)
	r := New(t.TempDir())
	r.Timeout = 2 * time.Second
	t.Cleanup(func() { r.Close() })
	w := &failoverv1.WorkloadSpec{Git: failoverv1.GitSpec{URL: stalled(t), Branch: "main"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := r.BranchHead(ctx, w)
	se, ok := stop.As(err)
	if !ok || !se.Wait || se.Reason != failoverv1.ReasonGitUnreachable || !strings.Contains(se.Message, "no answer within 2s") {
		t.Fatalf("err %v, want Waiting with GitUnreachable after the deadline", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("the fetch took %s", d)
	}
}

// A cancel of the run, such as Ctrl-C, stays a cancel: it is not a remote
// that did not answer.
func TestFetchCancelledIsNotUnreachable(t *testing.T) {
	isolate(t)
	r := New(t.TempDir())
	t.Cleanup(func() { r.Close() })
	w := &failoverv1.WorkloadSpec{Git: failoverv1.GitSpec{URL: stalled(t), Branch: "main"}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	_, err := r.BranchHead(ctx, w)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want the cancel", err)
	}
	if _, ok := stop.As(err); ok {
		t.Errorf("err %v is a stop", err)
	}
}

// fakeSSH puts an ssh on PATH that records its arguments and fails.
func fakeSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" >> '" + log + "'\nexit 255\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// #35 row 12. Over ssh, Tack sets a connect timeout and keepalives,
// unless the operator chose the ssh command, whose options Tack does not
// know.
func TestSSHTimeouts(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	for _, tt := range []struct {
		name string
		set  func(t *testing.T)
		want bool
	}{
		{name: "git's default ssh", set: func(*testing.T) {}, want: true},
		{name: "GIT_SSH_COMMAND", set: func(t *testing.T) { t.Setenv("GIT_SSH_COMMAND", "ssh -i /dev/null") }},
		{name: "core.sshCommand", set: func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "gitconfig")
			if err := os.WriteFile(cfg, []byte("[core]\n\tsshCommand = ssh -i /dev/null\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", cfg)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("GIT_SSH_COMMAND", "")
			os.Unsetenv("GIT_SSH_COMMAND")
			log := fakeSSH(t)
			tt.set(t)
			r := New(t.TempDir())
			t.Cleanup(func() { r.Close() })
			w := &failoverv1.WorkloadSpec{Git: failoverv1.GitSpec{URL: "ssh://git@gitops.example.invalid/gitops.git", Branch: "main"}}
			if _, err := r.BranchHead(context.Background(), w); err == nil {
				t.Fatal("the fetch over the fake ssh succeeded")
			}
			args, err := os.ReadFile(log)
			if err != nil {
				t.Fatalf("ssh did not run: %v", err)
			}
			got := strings.Contains(string(args), "-o ConnectTimeout=15 -o ServerAliveInterval=15 -o ServerAliveCountMax=3")
			if got != tt.want {
				t.Errorf("ssh ran with %q; Tack's options %v, want %v", strings.TrimSpace(string(args)), got, tt.want)
			}
		})
	}
}

// #35 row 12 (RFC0006 Requirement 3). Every git command carries the HTTP
// low-speed limit: a transfer that stalls ends before the deadline.
func TestGitCarriesLowSpeedLimit(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	isolate(t)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	rm := newRemote(t, map[string]string{"README.md": "gitops\n"})
	want := rm.head()
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := New(t.TempDir())
	t.Cleanup(func() { r.Close() })
	w := &failoverv1.WorkloadSpec{Git: failoverv1.GitSpec{URL: rm.url, Branch: "main"}}
	if head, err := r.BranchHead(context.Background(), w); err != nil || head != want {
		t.Fatalf("head %q, %v", head, err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	fetched := false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(line, "-c core.hooksPath=") {
			// Not a command of Repo.git, such as the read of core.sshCommand.
			continue
		}
		if !strings.Contains(line, "-c http.lowSpeedLimit=1 -c http.lowSpeedTime=30 ") {
			t.Errorf("git ran without the low-speed limit: %s", line)
		}
		fetched = fetched || strings.Contains(line, " fetch ")
	}
	if !fetched {
		t.Errorf("no fetch ran: %s", b)
	}
}
