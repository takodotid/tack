package cli

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/testenv"
	"github.com/takodotid/tack/internal/version"
)

// v131Ledger reads a ledger that tack v1.3.1 wrote, created at the time
// of its name. testdata/ledgers/v1.3.1 holds the ledgers that v1.3.1's own
// code left on the fakes of its CLI tests, with its version set to v1.3.1:
// a run from a to b that succeeded, ledger at b; a run back to a stopped at
// its first hold, then marked with tack abandon; and another run back to
// a, stopped at its first hold.
func v131Ledger(t *testing.T, name string) *failoverv1.SiteFailover {
	t.Helper()
	b, err := os.ReadFile("testdata/ledgers/v1.3.1/" + name + ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	var sf failoverv1.SiteFailover
	if err := yaml.UnmarshalStrict(b, &sf); err != nil {
		t.Fatal(err)
	}
	created, err := time.Parse("20060102-150405", strings.TrimPrefix(sf.Name, "pg-main-"))
	if err != nil {
		t.Fatal(err)
	}
	sf.CreationTimestamp = metav1.NewTime(created)
	return &sf
}

// Ledgers that v1.3.x wrote stay readable and usable under this tack
// ([RFC#0003] Requirements 13 and 24): tack status lists and shows them,
// tack abandon sees the mark, tack resume finishes the stopped run and
// keeps its history, and tack prune deletes the run that succeeded.
func TestLedgersOfV131(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "v1.4.0"
	succeeded, abandoned, stopped := v131Ledger(t, "succeeded"), v131Ledger(t, "abandoned"), v131Ledger(t, "stopped")
	for _, sf := range []*failoverv1.SiteFailover{succeeded, abandoned, stopped} {
		if sf.Status.WrittenBy != "v1.3.1" {
			t.Fatalf("%s: writtenBy %q, want v1.3.1", sf.Name, sf.Status.WrittenBy)
		}
	}
	// The sites as the run that succeeded left them: b is the primary.
	f := newFactory(map[string][]client.Object{
		"a": append(testenv.Site("a", "b", 0), abandoned, stopped),
		"b": append(testenv.Site("b", "b", 2), succeeded),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&testenv.Operator{Clients: f.clients, Sites: []string{"a", "b"}}).Run(ctx)

	out, code := run(t, f, "status", "-f", "testdata/workload.yaml")
	if code != ExitOK {
		t.Fatalf("status: exit %d:\n%s", code, out)
	}
	for name, state := range map[string]string{succeeded.Name: "Succeeded", abandoned.Name: "Abandoned", stopped.Name: "Stopped"} {
		if !slices.ContainsFunc(strings.Split(out, "\n"), func(l string) bool {
			f := strings.Fields(l)
			return len(f) > 5 && f[1] == name && f[5] == state
		}) {
			t.Errorf("status lists %s not as %s:\n%s", name, state, out)
		}
	}
	out, code = run(t, f, "status", stopped.Name, "-f", "testdata/workload.yaml")
	if code != ExitOK || !strings.Contains(out, "Stopped (StepFailed)  tack v1.3.1, user operator@example.com, --yes") {
		t.Errorf("status %s: exit %d:\n%s", stopped.Name, code, out)
	}
	out, code = run(t, f, "abandon", abandoned.Name, "-f", "testdata/workload.yaml", "--yes")
	if code != ExitOK || !strings.Contains(out, "was already abandoned at 2026-11-01T10:01:00Z: written by tack v1.3.1") {
		t.Errorf("abandon %s: exit %d:\n%s", abandoned.Name, code, out)
	}

	out, code = run(t, f, "resume", stopped.Name, "-f", "testdata/workload.yaml", "--yes")
	if code != ExitOK {
		t.Fatalf("resume %s: exit %d:\n%s", stopped.Name, code, out)
	}
	got := ledgerAt(t, f, "a", stopped.Name)
	var versions []string
	for _, h := range got.Status.History {
		versions = append(versions, string(h.State)+" "+h.Version)
	}
	if got.Status.State != failoverv1.RunSucceeded || got.Status.WrittenBy != "v1.4.0" ||
		strings.Join(versions, ", ") != "Running v1.3.1, Stopped v1.3.1, Running v1.4.0, Succeeded v1.4.0" {
		t.Errorf("after the resume: state %s, writtenBy %s, history %v", got.Status.State, got.Status.WrittenBy, versions)
	}

	out, code = run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "1", "--yes")
	if code != ExitOK || !strings.Contains(out, "deleted   ledger tack-system/"+succeeded.Name+" at site b\n") || !strings.Contains(out, "Deleted 1 ledgers") {
		t.Errorf("prune: exit %d:\n%s", code, out)
	}
	if left := append(runsAt(t, f, "a"), runsAt(t, f, "b")...); !slices.Equal(left, []string{stopped.Name, abandoned.Name}) {
		t.Errorf("after prune: %v, want the resumed and the abandoned run", left)
	}
}
