//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSignalHelper is the process the signal tests send signals to. It
// waits for the cancel, then outlives it, as a ledger write does.
func TestSignalHelper(t *testing.T) {
	if os.Getenv("TACK_SIGNAL_HELPER") == "" {
		t.Skip("run by TestSecondSignalEndsTack")
	}
	ctx, _ := signalContext()
	fmt.Println("ready")
	<-ctx.Done()
	fmt.Println("cancelled")
	time.Sleep(30 * time.Second)
	fmt.Println("finished")
	os.Exit(0)
}

// #35 row 38. The first SIGINT, SIGTERM or SIGHUP cancels the run, which
// then records why it stopped. The second ends tack at once, while it
// still records.
func TestSecondSignalEndsTack(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHelper$")
			cmd.Env = append(os.Environ(), "TACK_SIGNAL_HELPER=1")
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			lines := make(chan string, 10)
			go func() {
				s := bufio.NewScanner(out)
				for s.Scan() {
					lines <- s.Text()
				}
				close(lines)
			}()
			want := func(line string) {
				t.Helper()
				select {
				case got, ok := <-lines:
					if !ok || !strings.HasPrefix(got, line) {
						t.Fatalf("the helper printed %q (open %v), want %q", got, ok, line)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("the helper did not print %q", line)
				}
			}
			want("ready")
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			want("cancelled")
			// The handler is released by then, or very soon after.
			time.Sleep(100 * time.Millisecond)
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err == nil {
					t.Error("the helper exited 0 after the second signal")
				}
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				t.Fatal("the second signal did not end the helper")
			}
		})
	}
}
