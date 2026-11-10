package cli

import (
	"context"
	"errors"
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

// doneRun is a run that succeeded with no Deferred step, created age ago.
func doneRun(t *testing.T, name string, age time.Duration) *failoverv1.SiteFailover {
	t.Helper()
	sf := stoppedRun(t, name)
	sf.CreationTimestamp = metav1.NewTime(fixedNow.Add(-age))
	sf.Status.State, sf.Status.Reason = failoverv1.RunSucceeded, ""
	sf.Status.Steps = []failoverv1.StepStatus{{ID: "hold-a-cluster-pg-a", State: failoverv1.StepSucceeded}}
	return sf
}

// runsAt lists the names of the ledgers at a site.
func runsAt(t *testing.T, f *testFactory, site string) []string {
	t.Helper()
	items, err := (&ledger.Store{Client: f.clients.Sites[site], Namespace: "tack-system"}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, sf := range items {
		names = append(names, sf.Name)
	}
	return names
}

// #58 row 1 (#35 row 47). Ledgers were never deleted, and every search for
// runs reads all of them. tack prune deletes only runs that succeeded with
// no Deferred step, past the newest --keep that succeeded, and keeps every
// run that a rule of [RFC#0003] Requirement 27 keeps. An abandoned run,
// here the newest, does not count among the --keep: it is kept anyway.
func TestPrune(t *testing.T) {
	lastAbandoned := stoppedRun(t, "pg-main-last-abandoned")
	lastAbandoned.CreationTimestamp = metav1.NewTime(fixedNow.Add(-30 * time.Minute))
	lastAbandoned.Status.Abandoned = &failoverv1.Abandonment{At: metav1.NewTime(fixedNow), Reason: "by hand"}
	newest := doneRun(t, "pg-main-newest", 1*time.Hour)
	second := doneRun(t, "pg-main-second", 2*time.Hour)
	old := doneRun(t, "pg-main-old", 3*time.Hour)
	oldAtA := doneRun(t, "pg-main-old-at-a", 4*time.Hour)
	abandoned := stoppedRun(t, "pg-main-abandoned")
	abandoned.CreationTimestamp = metav1.NewTime(fixedNow.Add(-5 * time.Hour))
	abandoned.Status.Abandoned = &failoverv1.Abandonment{At: metav1.NewTime(fixedNow), Reason: "by hand"}
	marked := stoppedRun(t, "pg-main-marked")
	marked.CreationTimestamp = metav1.NewTime(fixedNow.Add(-6 * time.Hour))
	marked.Status.SupersededBy = "pg-main-taker"
	unmarked := stoppedRun(t, "pg-main-unmarked")
	unmarked.CreationTimestamp = metav1.NewTime(fixedNow.Add(-7 * time.Hour))
	taker := doneRun(t, "pg-main-taker", 6*time.Hour+30*time.Minute)
	taker.Spec.TakeOver, taker.Spec.TakeOvers = "pg-main-marked", []string{"pg-main-marked", "pg-main-unmarked"}
	rebuild := doneRun(t, "pg-main-rebuild", 8*time.Hour)
	rebuild.Status.FollowUps = []failoverv1.FollowUp{{Site: "a", Action: "Rebuild", Reason: "site loss"}}
	newer := doneRun(t, "pg-main-newer-tack", 9*time.Hour)
	newer.Status.WrittenBy = "v99.0.0"
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "v1.4.0"
	f := newFactory(map[string][]client.Object{
		"a": append(testenv.Site("a", "a", 2), oldAtA),
		"b": append(testenv.Site("b", "a", 0), lastAbandoned, newest, second, old, abandoned, marked, unmarked, taker, rebuild, newer),
	})
	out, code := run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "2", "--yes")
	if code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"Runs of workload pg-main, newest first; prune keeps the 2 newest that succeeded:\n",
		"pg-main-last-abandoned  Abandoned  2026-10-27 09:45:00Z  keep: abandoned\n",
		"pg-main-newest          Succeeded  2026-10-27 09:15:00Z  keep: one of the 2 newest that succeeded\n",
		"pg-main-second          Succeeded  2026-10-27 08:15:00Z  keep: one of the 2 newest that succeeded\n",
		"pg-main-old             Succeeded  2026-10-27 07:15:00Z  delete\n",
		"pg-main-abandoned       Abandoned  2026-10-27 05:15:00Z  keep: abandoned\n",
		"pg-main-marked          TakenOver  2026-10-27 04:15:00Z  keep: taken over\n",
		"pg-main-taker           Succeeded  2026-10-27 03:45:00Z  keep: only its spec marks run pg-main-unmarked as taken over\n",
		"pg-main-unmarked        TakenOver  2026-10-27 03:15:00Z  keep: taken over\n",
		"keep: follow-up Rebuild for site a: delete it by hand once that member was rebuilt\n",
		"keep: written by tack v99.0.0, newer than this tack v1.4.0\n",
		"deleted   ledger tack-system/pg-main-old-at-a at site a\n",
		"deleted   ledger tack-system/pg-main-old at site b\n",
		"Deleted 2 ledgers of workload pg-main; kept 9.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "pg-main-old-at-a at site a"), strings.Index(out, "pg-main-old at site b"); i < 0 || j < i {
		t.Errorf("prune did not delete the oldest run first:\n%s", out)
	}
	if got := runsAt(t, f, "a"); len(got) != 0 {
		t.Errorf("site a keeps %v", got)
	}
	got := strings.Join(runsAt(t, f, "b"), " ")
	for _, name := range []string{"pg-main-last-abandoned", "pg-main-newest", "pg-main-second", "pg-main-abandoned", "pg-main-marked", "pg-main-unmarked", "pg-main-taker", "pg-main-rebuild", "pg-main-newer-tack"} {
		if !strings.Contains(got+" ", name+" ") {
			t.Errorf("site b lost %s: %s", name, got)
		}
	}
	if strings.Contains(got+" ", "pg-main-old ") {
		t.Errorf("site b keeps pg-main-old: %s", got)
	}

	// Nothing left to delete: no question, nothing changes.
	out, code = run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "2")
	if code != ExitOK || !strings.Contains(out, "Nothing to delete.\n") {
		t.Errorf("second prune: exit %d:\n%s", code, out)
	}
}

// Prune deletes nothing unless every site answers and every run is
// finished, and the operator confirmed it.
func TestPruneRefuses(t *testing.T) {
	deferred := doneRun(t, "pg-main-deferred", time.Hour)
	deferred.Status.Steps = []failoverv1.StepStatus{{ID: "hold-a-cluster-pg-a", State: failoverv1.StepDeferred}}
	neverStarted := stoppedRun(t, "pg-main-never")
	neverStarted.Status = failoverv1.SiteFailoverStatus{}
	for _, tt := range []struct {
		name   string
		runs   []client.Object
		down   []string
		args   []string
		code   int
		want   string
		before func()
	}{
		{name: "a site gives no answer", down: []string{"a"}, code: ExitError,
			want: "prune reads every site, and site a gave no answer, so its runs were not read (site a: "},
		{name: "a run is stopped", runs: []client.Object{stoppedRun(t, "pg-main-stopped")}, code: ExitError,
			want: "not every run of the workload is finished: run pg-main-stopped at site b is Stopped (StepFailed). "},
		{name: "a run waits for its site", runs: []client.Object{deferred}, code: ExitError,
			want: "run pg-main-deferred at site b is Succeeded with a Deferred step"},
		{name: "a run never started", runs: []client.Object{neverStarted}, code: ExitError,
			want: "run pg-main-never at site b is not started"},
		{name: "keep 0", args: []string{"--keep", "0", "--yes"}, code: ExitUsage, want: "pass --keep N, 1 or more"},
		{name: "no keep", args: []string{"--yes"}, code: ExitUsage, want: "pass --keep N, 1 or more"},
		{name: "no terminal", args: []string{"--keep", "1"}, code: ExitUsage,
			want: "no terminal to confirm on: pass --yes once a human approved deleting these ledgers"},
		{name: "wrong name", args: []string{"--keep", "1"}, code: ExitError, want: "not confirmed; nothing was changed",
			before: func() {
				isTerminal = func() bool { return true }
				stdin = strings.NewReader("pg-other\n")
				t.Cleanup(func() { isTerminal, stdin = stdinIsTerminal, nil })
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runs := append([]client.Object{doneRun(t, "pg-main-new", time.Minute), doneRun(t, "pg-main-old", 2*time.Hour)}, tt.runs...)
			f := newFactory(map[string][]client.Object{
				"a": testenv.Site("a", "a", 2),
				"b": append(testenv.Site("b", "a", 0), runs...),
			}, tt.down...)
			args := tt.args
			if args == nil {
				args = []string{"--keep", "1", "--yes"}
			}
			if tt.before != nil {
				tt.before()
			}
			out, code := run(t, f, append([]string{"prune", "-f", "testdata/workload.yaml"}, args...)...)
			if code != tt.code || !strings.Contains(out, tt.want) {
				t.Errorf("exit %d, want %d with %q:\n%s", code, tt.code, tt.want, out)
			}
			f.clients.SetDown("a", false)
			if got := runsAt(t, f, "b"); len(got) != len(runs) {
				t.Errorf("site b holds %v, want all %d runs", got, len(runs))
			}
			if f.clients.Writes != 0 {
				t.Errorf("%d writes", f.clients.Writes)
			}
		})
	}
}

// A run that starts while the operator confirms makes the list stale:
// prune searches again after the confirmation, and deletes nothing.
func TestPruneSearchesAgainAfterConfirmation(t *testing.T) {
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), doneRun(t, "pg-main-new", time.Minute), doneRun(t, "pg-main-old", 2*time.Hour)),
	})
	typeAfter(t, func() {
		if err := testenv.CreateLedger(context.Background(), f.clients.Sites["b"], stoppedRun(t, "pg-main-started")); err != nil {
			t.Fatal(err)
		}
	})
	out, code := run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "1")
	if code != ExitError || !strings.Contains(out, "run pg-main-started at site b is Stopped") || !strings.Contains(out, "Nothing was deleted") {
		t.Errorf("exit %d, want 1 naming the new run:\n%s", code, out)
	}
	if got := runsAt(t, f, "b"); len(got) != 3 {
		t.Errorf("site b holds %v, want 3 runs", got)
	}

	// A ledger written while the operator confirms: its resourceVersion
	// changed, so prune deletes nothing.
	if err := f.clients.Sites["b"].Delete(context.Background(), ledgerAt(t, f, "b", "pg-main-started")); err != nil {
		t.Fatal(err)
	}
	typeAfter(t, func() {
		old := ledgerAt(t, f, "b", "pg-main-old")
		old.Status.Message = "written"
		if err := f.clients.Sites["b"].Status().Update(context.Background(), old); err != nil {
			t.Fatal(err)
		}
	})
	out, code = run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "1")
	if code != ExitError || !strings.Contains(out, "the runs changed since they were listed: the ledger of run pg-main-old at site b was written. Nothing was deleted") {
		t.Errorf("exit %d, want 1 naming the written ledger:\n%s", code, out)
	}
	if got := runsAt(t, f, "b"); len(got) != 2 {
		t.Errorf("site b holds %v, want 2 runs", got)
	}
}

// A delete that fails stops prune: the ledgers after it stay, and the
// error says how many were deleted.
func TestPruneStopsAtFirstError(t *testing.T) {
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), doneRun(t, "pg-main-new", time.Minute), doneRun(t, "pg-main-two", 2*time.Hour),
			doneRun(t, "pg-main-three", 3*time.Hour), doneRun(t, "pg-main-four", 4*time.Hour)),
	})
	f.clients.Sites["b"] = interceptor.NewClient(f.clients.Sites["b"].(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			if o.GetName() == "pg-main-three" {
				return errors.New("the API server refused")
			}
			return c.Delete(ctx, o, opts...)
		},
	})
	out, code := run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "1", "--yes")
	if code != ExitError || !strings.Contains(out, "delete the ledger of run pg-main-three at site b: the API server refused. Deleted 1 of 3 ledgers; the others stay") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	got := strings.Join(runsAt(t, f, "b"), " ")
	if got != "pg-main-new pg-main-two pg-main-three" {
		t.Errorf("site b holds %s, want pg-main-four deleted, and the rest kept", got)
	}
}
