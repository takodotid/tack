package cli

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
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
	rebuild.Status.StartedAt = &rebuild.CreationTimestamp
	rebuild.Status.FollowUps = []failoverv1.FollowUp{{Site: "a", Action: "Rebuild", Reason: "site loss"}}
	newer := doneRun(t, "pg-main-newer-tack", 9*time.Hour)
	newer.Status.WrittenBy = "v99.0.0"
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "v1.4.0"
	f := newFactory(map[string][]client.Object{
		// Site a's member was not created again after pg-main-rebuild.
		"a": {created(testenv.Cluster("a", "a", true, nil), fixedNow.Add(-9*time.Hour)), testenv.Pooler("a", 2), oldAtA},
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
		"keep: follow-up Rebuild for site a: its member was created at 2026-10-27T01:15:00Z, not 10s or more after the run started at 2026-10-27T02:15:00Z\n",
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

// #63. A tack whose version names no release, such as dev, keeps every run
// that a release wrote: it cannot tell which status fields those ledgers
// hold. A run that such a build wrote, or a release before v1.3.0, which
// recorded no version, it may delete.
func TestPruneByDevBuildKeepsRunsOfReleases(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "dev"
	release := doneRun(t, "pg-main-release", 3*time.Hour)
	release.Status.WrittenBy = "v1.4.0"
	dev := doneRun(t, "pg-main-dev", 4*time.Hour)
	dev.Status.WrittenBy = "dev"
	old := doneRun(t, "pg-main-old", 5*time.Hour)
	old.Status.WrittenBy = ""
	newest := doneRun(t, "pg-main-newest", time.Hour)
	newest.Status.WrittenBy = "dev"
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), newest, release, dev, old),
	})
	out, code := run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "1", "--yes")
	if code != ExitOK || !strings.Contains(out, "keep: written by tack v1.4.0, a release, and this tack dev names none\n") ||
		!strings.Contains(out, "Deleted 2 ledgers of workload pg-main; kept 2.") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for name, kept := range map[string]bool{"pg-main-newest": true, "pg-main-release": true, "pg-main-dev": false, "pg-main-old": false} {
		sf := &failoverv1.SiteFailover{}
		err := f.clients.Sites["b"].Get(context.Background(), client.ObjectKey{Namespace: "tack-system", Name: name}, sf)
		if (err == nil) != kept {
			t.Errorf("%s: read %v, want kept %v", name, err, kept)
		}
	}
}

// #66. prune ordered the runs that succeeded by metadata.creationTimestamp
// alone, which the API server of each ledger's site sets, to the second: a
// run of another site created a few seconds before the newest, by its
// site's clock, may be the newest. prune deletes such a run only when
// --keep runs were created after it by the rule of [RFC#0003] Requirement
// 18 (engine.CertainlyAfter), and keeps it otherwise, saying why.
func TestPruneOrdersRunsByTheClocks(t *testing.T) {
	for _, tt := range []struct {
		name string
		// site is pg-main-close's site; offset its creation time minus
		// that of pg-main-newest, at site b.
		site   string
		offset time.Duration
		// keep is pg-main-close's reason to stay, "" when it is deleted.
		keep string
	}{
		{name: "another site, 5 s before", site: "a", offset: -5 * time.Second,
			keep: "may be one of the 1 newest that succeeded: created less than 10s before run pg-main-newest at site b, by another site's clock"},
		{name: "another site, 9 s before", site: "a", offset: -9 * time.Second,
			keep: "may be one of the 1 newest that succeeded: created less than 10s before run pg-main-newest at site b, by another site's clock"},
		{name: "another site, 10 s before", site: "a", offset: -10 * time.Second},
		{name: "same site, same second", site: "b",
			keep: "may be one of the 1 newest that succeeded: created in the same second as run pg-main-newest"},
		{name: "same site, 1 s before", site: "b", offset: -time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			newest := doneRun(t, "pg-main-newest", time.Hour)
			near := doneRun(t, "pg-main-close", time.Hour-tt.offset)
			objects := map[string][]client.Object{
				"a": testenv.Site("a", "a", 2),
				"b": append(testenv.Site("b", "a", 0), newest, doneRun(t, "pg-main-old", 3*time.Hour)),
			}
			objects[tt.site] = append(objects[tt.site], near)
			f := newFactory(objects)
			out, code := run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "1", "--yes")
			if code != ExitOK {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			want := "delete"
			if tt.keep != "" {
				want = "keep: " + tt.keep
			}
			line := regexp.MustCompile(`(?m)^` + tt.site + ` +pg-main-close +Succeeded +\S+ \S+ +(.*)$`).FindStringSubmatch(out)
			if line == nil || line[1] != want {
				t.Errorf("pg-main-close: want %q:\n%s", want, out)
			}
			kept := slices.Contains(runsAt(t, f, tt.site), "pg-main-close")
			if kept != (tt.keep != "") {
				t.Errorf("pg-main-close kept %v, want %v:\n%s", kept, tt.keep != "", out)
			}
			if slices.Contains(runsAt(t, f, "b"), "pg-main-old") || !slices.Contains(runsAt(t, f, "b"), "pg-main-newest") {
				t.Errorf("site b holds %v, want pg-main-newest kept and pg-main-old deleted", runsAt(t, f, "b"))
			}
		})
	}
}

// #66. prune kept every run with a follow-up Rebuild, since it read no
// member, and the operator deleted such a run by hand. It now reads the
// member of the Rebuild's site, as tack plan does for [RFC#0003]
// Requirement 14, and deletes the run once the member was created again
// 10 s or more after the run started: plan no longer needs it. Anything it
// cannot tell keeps the run.
func TestPruneDeletesRunWhoseRebuildIsDone(t *testing.T) {
	started := fixedNow.Add(-3 * time.Hour)
	forbidden := func(c client.WithWatch) client.WithWatch {
		return interceptor.NewClient(c, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if key.Name == "pg-a" {
					return apierrors.NewForbidden(schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "clusters"}, key.Name, errors.New("no rule"))
				}
				return c.Get(ctx, key, o, opts...)
			},
		})
	}
	for _, tt := range []struct {
		name string
		// member is site a's Cluster, with its creation time.
		member *unstructured.Unstructured
		site   string
		wrap   func(client.WithWatch) client.WithWatch
		keep   string
	}{
		{name: "rebuilt an hour after the run started", member: created(testenv.Cluster("a", "b", false, nil), started.Add(time.Hour))},
		{name: "rebuilt 10 s after", member: created(testenv.Cluster("a", "b", false, nil), started.Add(10*time.Second))},
		{name: "created 5 s after, within the clocks' margin", member: created(testenv.Cluster("a", "b", false, nil), started.Add(5*time.Second)),
			keep: "follow-up Rebuild for site a: its member was created at 2026-10-27T07:15:05Z, not 10s or more after the run started at 2026-10-27T07:15:00Z"},
		{name: "created before the run", member: created(testenv.Cluster("a", "b", false, nil), started.Add(-time.Hour)),
			keep: "follow-up Rebuild for site a: its member was created at 2026-10-27T06:15:00Z, not 10s or more after the run started at 2026-10-27T07:15:00Z"},
		{name: "creation time unknown", member: testenv.Cluster("a", "b", false, nil),
			keep: "follow-up Rebuild for site a: its member's creation time is unknown"},
		{name: "member cannot be read", member: created(testenv.Cluster("a", "b", false, nil), started.Add(time.Hour)), wrap: forbidden,
			keep: `follow-up Rebuild for site a: its member could not be read (site a: read Cluster db/pg-a: clusters.postgresql.cnpg.io "pg-a" is forbidden: no rule)`},
		{name: "a site the Workload file does not name", member: created(testenv.Cluster("a", "b", false, nil), started.Add(time.Hour)), site: "c",
			keep: "follow-up Rebuild for site c: the Workload file has no such site"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			site := tt.site
			if site == "" {
				site = "a"
			}
			rebuild := doneRun(t, "pg-main-rebuild", 3*time.Hour)
			rebuild.Status.StartedAt = &metav1.Time{Time: started}
			rebuild.Status.FollowUps = []failoverv1.FollowUp{{Site: site, Action: engine.FollowUpRebuild, Reason: "promoted without a token"}}
			f := newFactory(map[string][]client.Object{
				"a": {tt.member, testenv.Pooler("a", 0)},
				"b": append(testenv.Site("b", "b", 2), doneRun(t, "pg-main-newest", time.Hour), rebuild),
			})
			if tt.wrap != nil {
				f.clients.Sites["a"] = tt.wrap(f.clients.Sites["a"].(client.WithWatch))
			}
			out, code := run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "1", "--yes")
			want := "pg-main-rebuild  Succeeded  2026-10-27 07:15:00Z  delete\n"
			if tt.keep != "" {
				want = "pg-main-rebuild  Succeeded  2026-10-27 07:15:00Z  keep: " + tt.keep + "\n"
			}
			if code != ExitOK || !strings.Contains(out, want) {
				t.Fatalf("exit %d, want 0 with %q:\n%s", code, want, out)
			}
			if kept := slices.Contains(runsAt(t, f, "b"), "pg-main-rebuild"); kept != (tt.keep != "") {
				t.Errorf("pg-main-rebuild kept %v, want %v", kept, tt.keep != "")
			}
		})
	}
}

// prune reads the members only for a run that it would otherwise delete
// and that has a follow-up Rebuild: a workload without one needs no get on
// its objects, as before.
func TestPruneReadsMembersOnlyForRebuild(t *testing.T) {
	newest := doneRun(t, "pg-main-newest", time.Hour)
	newest.Status.FollowUps = []failoverv1.FollowUp{{Site: "a", Action: engine.FollowUpRebuild, Reason: "promoted without a token"}}
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "b", 0),
		"b": append(testenv.Site("b", "b", 2), newest, doneRun(t, "pg-main-old", 3*time.Hour)),
	})
	reads := 0
	for _, s := range []string{"a", "b"} {
		f.clients.Sites[s] = interceptor.NewClient(f.clients.Sites[s].(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if key.Namespace == "db" {
					reads++
				}
				return c.Get(ctx, key, o, opts...)
			},
		})
	}
	out, code := run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "1", "--yes")
	if code != ExitOK || !strings.Contains(out, "Deleted 1 ledger of workload pg-main; kept 1.") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if reads != 0 {
		t.Errorf("prune read %d objects of the workload", reads)
	}
}

// A member created again while the operator confirms changes what prune
// does to a run with a follow-up Rebuild: prune searches again, reads the
// members again, and deletes nothing.
func TestPruneReadsMembersAgainAfterConfirmation(t *testing.T) {
	started := fixedNow.Add(-3 * time.Hour)
	rebuild := doneRun(t, "pg-main-rebuild", 3*time.Hour)
	rebuild.Status.StartedAt = &metav1.Time{Time: started}
	rebuild.Status.FollowUps = []failoverv1.FollowUp{{Site: "a", Action: engine.FollowUpRebuild, Reason: "promoted without a token"}}
	f := newFactory(map[string][]client.Object{
		"a": {created(testenv.Cluster("a", "b", false, nil), started.Add(-time.Hour)), testenv.Pooler("a", 0)},
		"b": append(testenv.Site("b", "b", 2), doneRun(t, "pg-main-newest", time.Hour), rebuild, doneRun(t, "pg-main-old", 4*time.Hour)),
	})
	typeAfter(t, func() {
		ctx := context.Background()
		if err := f.clients.Sites["a"].Delete(ctx, testenv.Cluster("a", "b", false, nil)); err != nil {
			t.Fatal(err)
		}
		if err := f.clients.Sites["a"].Create(ctx, created(testenv.Cluster("a", "b", false, nil), started.Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	})
	out, code := run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "1")
	if code != ExitError || !strings.Contains(out, "the runs changed since they were listed: what prune does to run pg-main-rebuild at site b changed, to delete. Nothing was deleted") {
		t.Errorf("exit %d, want 1 naming the run:\n%s", code, out)
	}
	if got := runsAt(t, f, "b"); len(got) != 3 {
		t.Errorf("site b holds %v, want 3 runs", got)
	}
}
