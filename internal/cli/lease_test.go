package cli

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/testenv"
	"github.com/takodotid/tack/internal/version"
)

// leaseFor returns a Lease of run sf, held by by and renewed at renewed:
// what a process that runs the run keeps at the ledger's site.
func leaseFor(sf *failoverv1.SiteFailover, by string, renewed time.Time) *coordinationv1.Lease {
	r, d := metav1.NewMicroTime(renewed), int32(30)
	return &coordinationv1.Lease{
		TypeMeta: metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"},
		ObjectMeta: metav1.ObjectMeta{Name: sf.Name, Namespace: "tack-system",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "failover.tako.id/v1", Kind: "SiteFailover", Name: sf.Name, UID: sf.UID}}},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &by, RenewTime: &r, AcquireTime: &r, LeaseDurationSeconds: &d},
	}
}

const otherProcess = "tack v9.9.9, process 7 on host-a"

// #65. tack abandon marked a run while a tack process still ran it, and a
// step in flight could land after the next run took over. The process
// holds the run's Lease: abandon refuses while it is fresh, before it
// asks and changing nothing; --force goes on, and the history records it.
// A Lease that expired, or that its process released, lets abandon mark
// the run.
func TestAbandonRefusesRunWithFreshLease(t *testing.T) {
	for _, tt := range []struct {
		name    string
		renewed time.Duration
		by      string
		force   bool
		refused bool
	}{
		{name: "renewed 5s ago", renewed: 5 * time.Second, by: otherProcess, refused: true},
		{name: "renewed 5s ago, --force", renewed: 5 * time.Second, by: otherProcess, force: true},
		{name: "expired", renewed: 41 * time.Second, by: otherProcess},
		{name: "released", renewed: time.Second, by: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sf := stoppedRun(t, "pg-main-earlier")
			sf.Status.State = failoverv1.RunRunning
			f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": append(testenv.Site("b", "a", 0), sf)})
			sf = ledgerAt(t, f, "b", "pg-main-earlier")
			if err := f.clients.Sites["b"].Create(context.Background(), leaseFor(sf, tt.by, fixedNow.Add(-tt.renewed))); err != nil {
				t.Fatal(err)
			}
			args := []string{"abandon", sf.Name, "-f", "testdata/workload.yaml", "--yes"}
			if tt.force {
				args = append(args, "--force")
			}
			out, code := run(t, f, args...)
			got := ledgerAt(t, f, "b", sf.Name)
			if tt.refused {
				if code != ExitError || !strings.Contains(out, "a tack process may still run the run: run pg-main-earlier is run by "+otherProcess+", whose lease on it was renewed at 2026-10-27T10:14:55Z and holds until 2026-10-27T10:15:35Z") ||
					!strings.Contains(out, "--force goes on all the same. Nothing was changed") || strings.Contains(out, "Type the workload name") {
					t.Fatalf("exit %d, want %d with the refusal before the question:\n%s", code, ExitError, out)
				}
				if got.Status.Abandoned != nil || got.ResourceVersion != sf.ResourceVersion {
					t.Errorf("the refused abandon changed the ledger: %+v", got.Status.Abandoned)
				}
				return
			}
			if code != ExitOK || got.Status.Abandoned == nil {
				t.Fatalf("exit %d, abandoned %v:\n%s", code, got.Status.Abandoned, out)
			}
			flags := got.Status.History[len(got.Status.History)-1].Flags
			if tt.force != slices.Contains(flags, "--force") || tt.force != strings.Contains(out, "  note      --force: going on, although run pg-main-earlier is run by "+otherProcess) {
				t.Errorf("--force %v: history flags %v:\n%s", tt.force, flags, out)
			}
		})
	}
}

// #65. tack run --take-over refuses a run whose Lease another process
// renewed a moment ago: that process may still change the workload. It
// refuses after the confirmation, before the ledger exists. --force goes
// on, and the new run's history records it; --force without --take-over
// is a usage error.
func TestTakeOverRefusesRunWithFreshLease(t *testing.T) {
	sf := stoppedRun(t, "pg-main-earlier")
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": append(testenv.Site("b", "a", 0), sf)})
	operate(t, f)
	sf = ledgerAt(t, f, "b", sf.Name)
	if err := f.clients.Sites["b"].Create(context.Background(), leaseFor(sf, otherProcess, fixedNow.Add(-5*time.Second))); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--take-over", sf.Name, "--yes")
	if code != ExitError || !strings.Contains(out, "--take-over pg-main-earlier, at site b: a tack process may still run the run: run pg-main-earlier is run by "+otherProcess) {
		t.Fatalf("exit %d, want %d with the refusal:\n%s", code, ExitError, out)
	}
	if runs := runsAt(t, f, "b"); !slices.Equal(runs, []string{sf.Name}) {
		t.Fatalf("the refused run created a ledger: %v", runs)
	}
	if out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--force", "--yes"); code != ExitUsage || !strings.Contains(out, "--force goes with --take-over") {
		t.Errorf("--force without --take-over: exit %d:\n%s", code, out)
	}
	out, code = run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--take-over", sf.Name, "--force", "--yes")
	if code != ExitOK {
		t.Fatalf("with --force: exit %d:\n%s", code, out)
	}
	h := ledgerAt(t, f, "b", "pg-main-20261027-101500").Status.History
	if !slices.Contains(h[0].Flags, "--force") || !slices.Contains(h[0].Flags, "--take-over="+sf.Name) {
		t.Errorf("the new run's first history entry has flags %v, want --force and --take-over", h[0].Flags)
	}
}

// v140Ledger reads a ledger that tack v1.4.0 wrote. testdata/ledgers/v1.4.0
// holds the ledgers that v1.4.0's own code left on the fakes of its CLI
// tests, with its version set to v1.4.0 and its clock at the time of each
// run's name: a run from a to b that succeeded, ledger at b; a run back to
// a, stopped at its first hold; and a run from a to b whose process died as
// it started wait-pooler-a, which its ledger shows Running.
func v140Ledger(t *testing.T, name string) *failoverv1.SiteFailover {
	t.Helper()
	b, err := os.ReadFile("testdata/ledgers/v1.4.0/" + name + ".yaml")
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

// #65. Ledgers that v1.4.0 wrote have no Lease, and stay usable under this
// tack: tack status lists them, tack resume finishes the stopped run and
// holds its Lease, and tack abandon judges the Running run by its ledger:
// it refuses while its Running step's timeout and 5 minutes have not
// passed, and marks it once they have.
func TestLedgersOfV140(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "v1.4.0-1-gd483d80"
	succeeded, stopped, running := v140Ledger(t, "succeeded"), v140Ledger(t, "stopped"), v140Ledger(t, "running")
	for _, sf := range []*failoverv1.SiteFailover{succeeded, stopped, running} {
		if sf.Status.WrittenBy != "v1.4.0" {
			t.Fatalf("%s: writtenBy %q, want v1.4.0", sf.Name, sf.Status.WrittenBy)
		}
	}
	// The sites as the run that succeeded left them: b is the primary.
	f := newFactory(map[string][]client.Object{
		"a": append(testenv.Site("a", "b", 0), stopped),
		"b": append(testenv.Site("b", "b", 2), succeeded),
	})
	operate(t, f)
	out, code := run(t, f, "status", "-f", "testdata/workload.yaml")
	if code != ExitOK {
		t.Fatalf("status: exit %d:\n%s", code, out)
	}
	for name, state := range map[string]string{succeeded.Name: "Succeeded", stopped.Name: "Stopped"} {
		if !slices.ContainsFunc(strings.Split(out, "\n"), func(l string) bool {
			f := strings.Fields(l)
			return len(f) > 5 && f[1] == name && f[5] == state
		}) {
			t.Errorf("status lists %s not as %s:\n%s", name, state, out)
		}
	}
	if out, code := run(t, f, "resume", stopped.Name, "-f", "testdata/workload.yaml", "--yes"); code != ExitOK {
		t.Fatalf("resume %s: exit %d:\n%s", stopped.Name, code, out)
	}
	got := ledgerAt(t, f, "a", stopped.Name)
	var versions []string
	for _, h := range got.Status.History {
		versions = append(versions, string(h.State)+" "+h.Version)
	}
	if got.Status.State != failoverv1.RunSucceeded || strings.Join(versions, ", ") != "Running v1.4.0, Stopped v1.4.0, Running v1.4.0-1-gd483d80, Succeeded v1.4.0-1-gd483d80" {
		t.Errorf("after the resume: state %s, history %v", got.Status.State, versions)
	}
	l := &coordinationv1.Lease{}
	if err := f.clients.Sites["a"].Get(context.Background(), client.ObjectKey{Namespace: "tack-system", Name: stopped.Name}, l); err != nil || *l.Spec.HolderIdentity != "" {
		t.Errorf("the resumed run's Lease: %v, %+v", err, l.Spec)
	}

	// The run whose process died as it started wait-pooler-a, which waits
	// up to 10m, at 10:00:00.
	f = newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": append(testenv.Site("b", "a", 0), running)})
	if out, code := run(t, f, "status", "-f", "testdata/workload.yaml"); code != ExitOK || !strings.Contains(out, running.Name+"  Planned  a     b   Running") {
		t.Errorf("status: exit %d:\n%s", code, out)
	}
	abandon := func(at string) (string, int) {
		tm, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}
		defer func(n func() time.Time) { nowFunc = n }(nowFunc)
		nowFunc = func() time.Time { return tm }
		var out strings.Builder
		code := Execute(context.Background(), f, []string{"abandon", running.Name, "-f", "testdata/workload.yaml", "--yes"}, &out, &out)
		return out.String(), code
	}
	for _, at := range []string{"2026-11-11T10:02:00Z", "2026-11-11T10:14:59Z"} {
		if out, code := abandon(at); code != ExitError || !strings.Contains(out, "run pg-main-20261111-100000 is Running with no lease that tells, and its ledger was last written at 2026-11-11T10:00:00Z by tack v1.4.0: a tack process may still run it, in step wait-pooler-a, until 2026-11-11T10:15:00Z") {
			t.Fatalf("abandon at %s: exit %d, want %d with the refusal:\n%s", at, code, ExitError, out)
		}
	}
	if out, code := abandon("2026-11-11T10:15:00Z"); code != ExitOK || ledgerAt(t, f, "b", running.Name).Status.Abandoned == nil {
		t.Fatalf("abandon at 10:15: exit %d:\n%s", code, out)
	}
}
