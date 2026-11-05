//go:build linux

package gitops

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/stop"
)

// droppingConnect returns the URL of an HTTPS remote whose TCP connect
// never completes, as a host that drops the connect does: a listener with
// a full accept queue, so Linux drops each new SYN.
func droppingConnect(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	port := sa.(*syscall.SockaddrInet4).Port
	for i := 0; i < 4; i++ {
		c, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { syscall.Close(c) })
		_ = syscall.Connect(c, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}, Port: port})
	}
	time.Sleep(200 * time.Millisecond)
	return fmt.Sprintf("https://127.0.0.1:%d/gitops.git", port)
}

// PR #49's finding (RFC0006 Limitations). git has no connect timeout for
// HTTP, and libcurl's low-speed check starts after the connect: a fetch
// from a host that drops the TCP connect runs until the command's
// deadline, then counts as unreachable.
func TestFetchFromHostThatDropsTheConnectIsUnreachable(t *testing.T) {
	isolate(t)
	r := New(t.TempDir())
	r.Timeout = 3 * time.Second
	t.Cleanup(func() { r.Close() })
	w := &failoverv1.WorkloadSpec{Git: failoverv1.GitSpec{URL: droppingConnect(t), Branch: "main"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := r.BranchHead(ctx, w)
	se, ok := stop.As(err)
	if !ok || !se.Wait || se.Reason != failoverv1.ReasonGitUnreachable || !strings.Contains(se.Message, "no answer within 3s") {
		t.Fatalf("err %v, want Waiting with GitUnreachable at the deadline", err)
	}
	if d := time.Since(start); d < 3*time.Second || d > 20*time.Second {
		t.Errorf("the fetch ended after %s, want the deadline", d)
	}
}
