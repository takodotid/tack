// Package measure holds the script that measures how long a run keeps writes down, as a client sees it.
package measure

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The report names the longest time without an acknowledged write, and the writes around it.
func TestReport(t *testing.T) {
	log := filepath.Join(t.TempDir(), "writes.log")
	lines := "2026-11-13T23:59:59.000Z ok 1\n2026-11-13T23:59:59.200Z ok 2\n2026-11-13T23:59:59.400Z fail 3\n" +
		"2026-11-14T00:00:05.400Z fail 4\n2026-11-14T00:00:07.600Z ok 5\n2026-11-14T00:00:07.800Z ok 6\n"
	if err := os.WriteFile(log, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("./writes.sh", "report", log).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, want := range []string{
		"acknowledged writes: 4; failed attempts: 2",
		"longest time without an acknowledged write: 8.4 s, after 2026-11-13T23:59:59.200Z, until 2026-11-14T00:00:07.600Z",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("report:\n%s\nwant %q", out, want)
		}
	}
}
