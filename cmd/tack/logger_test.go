package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Observed on kind: a command that made a client more than 30 s after it
// started, such as tack status while a credential plugin waited for the
// operator, printed "[controller-runtime] log.SetLogger(...) was never
// called" and a stack trace on stderr. The e2e's output had it too.
func TestNoControllerRuntimeLoggerWarning(t *testing.T) {
	if os.Getenv("TACK_TEST_LATE_CLIENT") == "1" {
		time.Sleep(31 * time.Second)
		_, _ = client.New(&rest.Config{Host: "http://127.0.0.1:1"}, client.Options{})
		return
	}
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNoControllerRuntimeLoggerWarning$", "-test.v")
	cmd.Env = append(os.Environ(), "TACK_TEST_LATE_CLIENT=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if strings.Contains(string(out), "log.SetLogger(...) was never called") {
		t.Errorf("a client made after 30 s printed controller-runtime's warning:\n%s", out)
	}
}
