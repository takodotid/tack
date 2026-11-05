package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/testenv"
	"github.com/takodotid/tack/internal/version"
)

// heldRun is a stopped run that still holds Cluster pg-a at site a.
func heldRun(t *testing.T, name string) *failoverv1.SiteFailover {
	sf := stoppedRun(t, name)
	held := metav1.NewTime(fixedNow.Add(-30 * time.Minute))
	sf.Status.Held = []failoverv1.HeldObject{{Site: "a", HeldAt: &held,
		Object:        failoverv1.ObjectRef{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Namespace: "db", Name: "pg-a"},
		Kustomization: failoverv1.ObjectKey{Namespace: "flux-system", Name: "a"}}}
	return sf
}

func ledgerAt(t *testing.T, f *testFactory, site, name string) *failoverv1.SiteFailover {
	t.Helper()
	sf, err := (&ledger.Store{Client: f.clients.Sites[site], Namespace: "tack-system"}).Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return sf
}

// PR #44: an abandoned run had no durable mark, so it could be resumed
// until a new run took it over. tack abandon records status.abandoned:
// tack resume then refuses the run, and a new run no longer waits for it.
// Its holds stay, and the output names them.
func TestAbandon(t *testing.T) {
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), heldRun(t, "pg-main-earlier")),
	})
	f.flux.Held["a/Cluster/pg-a"] = "pg-main-earlier"
	out, code := run(t, f, "abandon", "pg-main-earlier", "-f", "testdata/workload.yaml", "--reason", "a promoted again by hand", "--yes")
	if code != ExitOK {
		t.Fatalf("abandon: exit %d:\n%s", code, out)
	}
	for _, want := range []string{"Cluster db/pg-a at site a", "(" + version.DocLink("docs/runbooks/abandon-run.md") + ")", "--take-over pg-main-earlier", "run pg-main-earlier abandoned"} {
		if !strings.Contains(out, want) {
			t.Errorf("abandon: output lacks %q:\n%s", want, out)
		}
	}
	sf := ledgerAt(t, f, "b", "pg-main-earlier")
	if a := sf.Status.Abandoned; a == nil || a.Reason != "a promoted again by hand" || !a.At.Equal(&metav1.Time{Time: fixedNow}) {
		t.Fatalf("status.abandoned %+v", a)
	}
	if sf.Status.State != failoverv1.RunStopped || f.flux.Held["a/Cluster/pg-a"] != "pg-main-earlier" {
		t.Errorf("abandon changed more than its mark: state %s, hold %q", sf.Status.State, f.flux.Held["a/Cluster/pg-a"])
	}

	out, code = run(t, f, "resume", "pg-main-earlier", "-f", "testdata/workload.yaml", "--yes")
	if code != ExitError || !strings.Contains(out, "run pg-main-earlier was abandoned") {
		t.Fatalf("resume: exit %d, want %d with a refusal:\n%s", code, ExitError, out)
	}
	out, code = run(t, f, "status", "-f", "testdata/workload.yaml")
	if code != ExitOK || !strings.Contains(out, "Abandoned") {
		t.Fatalf("status: exit %d:\n%s", code, out)
	}
	// A new run is not refused; it stops on the hold the abandoned run left.
	out, code = run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--yes")
	if code != ExitStopped || !strings.Contains(out, "HeldByAnotherRun") {
		t.Fatalf("run: exit %d, want %d on the hold:\n%s", code, ExitStopped, out)
	}

	out, code = run(t, f, "abandon", "pg-main-earlier", "-f", "testdata/workload.yaml", "--yes")
	if code != ExitOK || !strings.Contains(out, "was already abandoned") {
		t.Fatalf("abandon again: exit %d:\n%s", code, out)
	}
}

// tack abandon refuses a run that succeeded with no step left, and asks
// before it writes: without a terminal and --yes it changes nothing.
func TestAbandonRefuses(t *testing.T) {
	done := stoppedRun(t, "pg-main-done")
	done.Status.State, done.Status.Reason = failoverv1.RunSucceeded, ""
	done.Status.Steps[0].State = failoverv1.StepSucceeded
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), done, heldRun(t, "pg-main-earlier")),
	})
	out, code := run(t, f, "abandon", "pg-main-done", "-f", "testdata/workload.yaml", "--yes")
	if code != ExitError || !strings.Contains(out, "nothing to abandon") {
		t.Errorf("abandon a run that succeeded: exit %d:\n%s", code, out)
	}
	out, code = run(t, f, "abandon", "pg-main-earlier", "-f", "testdata/workload.yaml")
	if code != ExitUsage || !strings.Contains(out, "--yes") {
		t.Errorf("abandon without a terminal: exit %d:\n%s", code, out)
	}
	for _, name := range []string{"pg-main-done", "pg-main-earlier"} {
		if a := ledgerAt(t, f, "b", name).Status.Abandoned; a != nil {
			t.Errorf("run %s abandoned: %+v", name, a)
		}
	}
}

// A ledger site whose CRD is older than v1.3.0 drops status.abandoned on
// the write. tack abandon reads the ledger back and says so.
func TestAbandonWithOldCRD(t *testing.T) {
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), heldRun(t, "pg-main-earlier")),
	})
	f.clients.SetSite("b", interceptor.NewClient(f.clients.Sites["b"].(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
			if sf, ok := o.(*failoverv1.SiteFailover); ok {
				sf.Status.Abandoned = nil
			}
			return c.SubResource(sub).Update(ctx, o, opts...)
		},
	}))
	out, code := run(t, f, "abandon", "pg-main-earlier", "-f", "testdata/workload.yaml", "--yes")
	if code != ExitError || !strings.Contains(out, "apply this version's CRD there") {
		t.Fatalf("abandon: exit %d:\n%s", code, out)
	}
}

// PR #44: tack status without a run listed a run that another run took
// over as its last state, with no sign of the take-over. It lists it as
// TakenOver, whether its own ledger or the other run's spec records it.
func TestStatusListsRunTakenOver(t *testing.T) {
	marked := stoppedRun(t, "pg-main-one")
	marked.Status.SupersededBy = "pg-main-three"
	unmarked := stoppedRun(t, "pg-main-two")
	taker := stoppedRun(t, "pg-main-three")
	taker.Spec.TakeOver = "pg-main-two"
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), marked, unmarked, taker),
	})
	out, code := run(t, f, "status", "-f", "testdata/workload.yaml")
	if code != ExitOK {
		t.Fatalf("status: exit %d:\n%s", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		want := map[string]string{"pg-main-one": "TakenOver", "pg-main-two": "TakenOver", "pg-main-three": "Stopped"}[f[1]]
		if want != "" && f[5] != want {
			t.Errorf("run %s listed as %s, want %s:\n%s", f[1], f[5], want, out)
		}
	}
	if !strings.Contains(out, "pg-main-three") {
		t.Errorf("status lacks the runs:\n%s", out)
	}
}

// #59, observed in a two-site test of v1.3.0: after the runbook removed
// every hold, tack abandon listed each object "(held)" and said "the run's
// holds stay after it is abandoned", from the ledger. It says that the
// list is the ledger's record, reads and changes no object of the
// workload, and says that kubectl get sfo still shows the run's state.
func TestAbandonListsHoldsAsRecorded(t *testing.T) {
	for _, tt := range []struct {
		name  string
		state failoverv1.RunState
		want  string
	}{
		{name: "stopped", state: failoverv1.RunStopped, want: "kubectl get sfo still shows its State as Stopped: "},
		{name: "never started", want: "kubectl get sfo still shows its State empty: "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sf := heldRun(t, "pg-main-earlier")
			sf.Status.State = tt.state
			// The runbook removed the hold: the object holds none.
			f := newFactory(map[string][]client.Object{
				"a": testenv.Site("a", "a", 2),
				"b": append(testenv.Site("b", "a", 0), sf),
			})
			out, code := run(t, f, "abandon", "pg-main-earlier", "-f", "testdata/workload.yaml", "--reason", "holds removed by hand", "--yes")
			if code != ExitOK {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			for _, want := range []string{
				"Held objects, as the ledger records them:\n  site a: Cluster db/pg-a (held)\n",
				"The ledger records these objects as held by the run, and not released:\n  Cluster db/pg-a at site a\n",
				"Abandon does not read the objects: a hold removed by hand is still listed.",
				tt.want,
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "holds stay after it is abandoned") {
				t.Errorf("output says the holds stay:\n%s", out)
			}
			if len(f.flux.Calls) > 0 || len(f.flux.Held) > 0 {
				t.Errorf("abandon acted on the workload's objects: calls %v, holds %v", f.flux.Calls, f.flux.Held)
			}
			if a := ledgerAt(t, f, "b", "pg-main-earlier").Status.Abandoned; a == nil || !a.At.Equal(&metav1.Time{Time: fixedNow}) {
				t.Errorf("status.abandoned %+v", a)
			}
		})
	}
}
