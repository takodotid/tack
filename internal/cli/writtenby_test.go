package cli

import (
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/internal/testenv"
	"github.com/takodotid/tack/internal/version"
)

// #35 row 26 (RFC0003 Requirement 24). A ledger that a newer tack wrote
// is refused by tack resume and tack abandon of an older release, before
// they ask the operator and with nothing changed; tack status reads it,
// and says which tack wrote it.
func TestOlderTackRefusesNewerLedger(t *testing.T) {
	refusesNewerLedger(t, "v1.3.0", "which is older", "Written by: tack v1.4.0, newer than this tack v1.3.0: tack resume and tack abandon refuse the run")
}

// #63. A build that names no release, such as one of go install without
// a version, or go build without the release's ldflags, refuses a ledger
// that a release wrote, as an older release does. v1.4.0's dev build
// resumed it, and its writes dropped the status fields it did not know.
func TestDevBuildRefusesLedgerOfRelease(t *testing.T) {
	refusesNewerLedger(t, "dev", "which names no release", "Written by: tack v1.4.0, a release, and this tack dev names none: tack resume and tack abandon refuse the run")
}

func refusesNewerLedger(t *testing.T, tack, which, status string) {
	old := version.Version
	version.Version = tack
	t.Cleanup(func() { version.Version = old })
	sf := stoppedRun(t, "pg-main-earlier")
	sf.Status.WrittenBy = "v1.4.0"
	for _, args := range [][]string{
		{"resume", "pg-main-earlier"},
		{"abandon", "pg-main-earlier"},
	} {
		t.Run(args[0], func(t *testing.T) {
			f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": append(testenv.Site("b", "a", 0), sf.DeepCopy())})
			before := ledgerAt(t, f, "b", "pg-main-earlier").ResourceVersion
			isTerminal = func() bool { return false }
			defer func() { isTerminal = stdinIsTerminal }()
			out, code := run(t, f, append(args, "-f", "testdata/workload.yaml")...)
			if code != ExitError || !strings.Contains(out, "run pg-main-earlier was last written by tack v1.4.0, and this tack is "+tack+", "+which) ||
				!strings.Contains(out, "Nothing was changed: use tack v1.4.0 or later") {
				t.Fatalf("exit %d, want %d with the refusal:\n%s", code, ExitError, out)
			}
			if f.clients.Writes != 0 || len(f.flux.Calls) != 0 || ledgerAt(t, f, "b", "pg-main-earlier").ResourceVersion != before {
				t.Errorf("%d cluster writes, Flux %v", f.clients.Writes, f.flux.Calls)
			}
		})
	}
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": append(testenv.Site("b", "a", 0), sf.DeepCopy())})
	out, code := run(t, f, "status", "pg-main-earlier", "-f", "testdata/workload.yaml")
	if code != ExitOK || !strings.Contains(out, status) {
		t.Errorf("status: exit %d:\n%s", code, out)
	}
}

// RFC0003 Requirement 24: a run that takes over a run whose ledger a
// newer tack wrote does not mark it, says so, and goes on: its own spec
// records the take-over (Requirement 17). A build that names no release
// does the same with a release's ledger (#63).
func TestTakeOverOfNewerLedgerIsNotMarked(t *testing.T) {
	for _, tack := range []string{"v1.3.0", "dev"} {
		t.Run(tack, func(t *testing.T) { takeOverOfNewerLedgerIsNotMarked(t, tack) })
	}
}

func takeOverOfNewerLedgerIsNotMarked(t *testing.T, tack string) {
	old := version.Version
	version.Version = tack
	t.Cleanup(func() { version.Version = old })
	sf := stoppedRun(t, "pg-main-earlier")
	sf.Status.WrittenBy = "v1.4.0"
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": append(testenv.Site("b", "a", 0), sf)})
	operate(t, f)
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--take-over", "pg-main-earlier", "--yes")
	if code != ExitOK || !strings.Contains(out, "run pg-main-earlier is not marked as taken over (a newer tack wrote the ledger: run pg-main-earlier was last written by tack v1.4.0") {
		t.Fatalf("run: exit %d:\n%s", code, out)
	}
	if got := ledgerAt(t, f, "b", "pg-main-earlier").Status; got.SupersededBy != "" || got.WrittenBy != "v1.4.0" {
		t.Errorf("the newer ledger was written: supersededBy %q, writtenBy %q", got.SupersededBy, got.WrittenBy)
	}
	if got := ledgerAt(t, f, "b", "pg-main-20261027-101500").Status.WrittenBy; got != tack {
		t.Errorf("the new run's writtenBy %q, want %s", got, tack)
	}
}
