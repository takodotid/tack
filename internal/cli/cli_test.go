package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/config"
	"github.com/takodotid/tack/internal/driver"
	_ "github.com/takodotid/tack/internal/driver/cnpg"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/testenv"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var fixedNow = time.Date(2026, 10, 27, 10, 15, 0, 0, time.UTC)

type testFactory struct {
	clients *testenv.Clients
	flux    *testenv.Flux
	git     *testenv.Git
}

func (f *testFactory) Clients(string, map[string]string) kube.Clients { return f.clients }

func (f *testFactory) Env(_ context.Context, _ string, spec *failoverv1.WorkloadSpec, ledgerSite string, out io.Writer) (*engine.Env, func(), error) {
	d, err := driver.Get(spec.Driver)
	if err != nil {
		return nil, nil, err
	}
	env := &engine.Env{
		Clients: f.clients, Driver: d, Flux: f.flux, Git: f.git, Out: out,
		Now: func() time.Time { return fixedNow }, Poll: time.Millisecond,
		ReachWaits: testWaits,
	}
	if cl, err := f.clients.Client(ledgerSite); err == nil {
		env.Ledger = &ledger.Store{Client: cl, Namespace: spec.Ledger.Namespace}
	}
	return env, func() {}, nil
}

func newFactory(objects map[string][]client.Object, down ...string) *testFactory {
	f := &testFactory{clients: testenv.NewClients(objects), flux: testenv.NewFlux("main@sha1:1111111111111111111111111111111111111111"), git: &testenv.Git{}}
	for _, s := range down {
		f.clients.SetDown(s, true)
	}
	return f
}

// testWaits are the pauses between the checks of a site in tests.
var testWaits = []time.Duration{time.Millisecond, time.Millisecond}

func run(t *testing.T, f Factory, args ...string) (string, int) {
	t.Helper()
	nowFunc = func() time.Time { return fixedNow }
	reachWaits = testWaits
	var out bytes.Buffer
	code := Execute(context.Background(), f, args, &out, &out)
	return out.String(), code
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/cli -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file; run go test ./internal/cli -update and review the diff\n--- got ---\n%s", path, got)
	}
}

func TestPlanGolden(t *testing.T) {
	tests := []struct {
		name    string
		objects map[string][]client.Object
		down    []string
		blip    string
		gitDown bool
		args    []string
		code    int
		// checks are the checks of a site's API the plan may make.
		checks map[string]int
	}{
		{
			name:    "plan-planned",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b"},
		},
		{
			name:    "plan-site-loss",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			down:    []string{"a"},
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss"},
			checks:  map[string]int{"a": 1},
		},
		{
			name:    "plan-site-loss-git-down",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			down:    []string{"a"},
			gitDown: true,
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss"},
		},
		{
			name:    "plan-planned-git-down",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			gitDown: true,
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b"},
			code:    ExitError,
		},
		{
			name: "plan-three-sites-strait",
			objects: map[string][]client.Object{
				"a": {testenv.Cluster("a", "a", true, nil)},
				"b": {testenv.Cluster("b", "a", false, nil)},
				"c": {testenv.Cluster("c", "a", false, nil)},
			},
			args: []string{"plan", "-f", "testdata/workload-3.yaml", "--to", "b"},
		},
		{
			name: "plan-three-sites-c-down",
			objects: map[string][]client.Object{
				"a": {testenv.Cluster("a", "a", true, nil)},
				"b": {testenv.Cluster("b", "a", false, nil)},
				"c": {testenv.Cluster("c", "a", false, nil)},
			},
			down:   []string{"c"},
			args:   []string{"plan", "-f", "testdata/workload-3.yaml", "--to", "b"},
			checks: map[string]int{"a": 1, "b": 1, "c": 3},
		},
		{
			name:    "plan-planned-from-down",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			down:    []string{"a"},
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b"},
			code:    ExitError,
			checks:  map[string]int{"a": 3},
		},
		{
			name:    "plan-planned-blip",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			blip:    "b",
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b"},
			checks:  map[string]int{"a": 1, "b": 2},
		},
		{
			name:    "plan-site-loss-from-primary",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss"},
			code:    ExitError,
		},
		{
			name: "plan-planned-not-rebuilt",
			objects: map[string][]client.Object{
				"a": {testenv.Cluster("a", "b", false, nil), testenv.Pooler("a", 0)},
				"b": {testenv.Cluster("b", "b", true, nil), testenv.Pooler("b", 2), siteLossRun("a", fixedNow.Add(-time.Hour))},
			},
			args: []string{"plan", "-f", "testdata/workload.yaml", "--to", "a"},
			code: ExitError,
		},
		{
			name: "plan-planned-rebuilt",
			objects: map[string][]client.Object{
				"a": {created(testenv.Cluster("a", "b", false, nil), fixedNow.Add(-time.Minute)), testenv.Pooler("a", 0)},
				"b": {testenv.Cluster("b", "b", true, nil), testenv.Pooler("b", 2), siteLossRun("a", fixedNow.Add(-time.Hour))},
			},
			args: []string{"plan", "-f", "testdata/workload.yaml", "--to", "a"},
		},
		{
			name:    "plan-bad-mode",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "fast"},
			code:    ExitUsage,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFactory(tt.objects, tt.down...)
			f.git.Unreachable = tt.gitDown
			if tt.blip != "" {
				f.clients.Blip(tt.blip, 1)
			}
			out, code := run(t, f, tt.args...)
			if code != tt.code {
				t.Fatalf("exit code %d, want %d\n%s", code, tt.code, out)
			}
			golden(t, tt.name, out)
			for site, n := range tt.checks {
				if got := f.clients.Checks[site]; got != n {
					t.Errorf("site %s checked %d times, want %d", site, got, n)
				}
			}
			if len(f.flux.Calls) > 0 || len(f.git.Commits) > 0 || f.clients.Writes > 0 {
				t.Errorf("plan changed something: Flux %v, Git %d commits, %d cluster writes", f.flux.Calls, len(f.git.Commits), f.clients.Writes)
			}
		})
	}
}

// siteLossRun is a succeeded site-loss run that left site lost to rebuild.
func siteLossRun(lost string, started time.Time) *failoverv1.SiteFailover {
	t := metav1.NewTime(started)
	return &failoverv1.SiteFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-main-earlier", Namespace: "tack-system", Labels: map[string]string{engine.LabelWorkload: "pg-main"}},
		Spec:       failoverv1.SiteFailoverSpec{Mode: failoverv1.ModeSiteLoss, From: lost, To: "b"},
		Status: failoverv1.SiteFailoverStatus{
			State: failoverv1.RunSucceeded, StartedAt: &t,
			FollowUps: []failoverv1.FollowUp{{Site: lost, Action: engine.FollowUpRebuild, Reason: "promoted without a token"}},
		},
	}
}

// created sets an object's creation time, as a rebuild does.
func created(u *unstructured.Unstructured, at time.Time) *unstructured.Unstructured {
	u.SetCreationTimestamp(metav1.NewTime(at))
	return u
}

// The check before a run starts searches every site for runs again. A
// blip at a site must not hide a run there that is not Succeeded.
func TestRunSearchRetried(t *testing.T) {
	active := siteLossRun("a", fixedNow.Add(-time.Hour))
	active.Status.State, active.Status.FollowUps = failoverv1.RunStopped, nil
	f := newFactory(map[string][]client.Object{"a": append(testenv.Site("a", "a", 2), active), "b": testenv.Site("b", "a", 0)})
	// The plan's check of site a answers; the next one fails once.
	f.clients.Flap("a", true, false)
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--yes")
	if code != ExitError || !strings.Contains(out, "run pg-main-earlier at site a is Stopped") {
		t.Fatalf("exit %d, want %d with the run at site a:\n%s", code, ExitError, out)
	}
	if n := f.clients.Checks["a"]; n != 3 {
		t.Errorf("site a checked %d times, want 3: 1 by the plan, 2 by the search before the run", n)
	}
	if len(f.flux.Calls) > 0 {
		t.Errorf("the run started: Flux %v", f.flux.Calls)
	}
}

// The search for a named run checks a site again when the run was not
// found elsewhere, and only then. The search for every run checks each
// site up to 3 times.
func TestStatusSearchRetried(t *testing.T) {
	objects := func() map[string][]client.Object {
		return map[string][]client.Object{
			"a": testenv.Site("a", "b", 0),
			"b": append(testenv.Site("b", "b", 2), siteLossRun("a", fixedNow.Add(-time.Hour))),
		}
	}
	tests := []struct {
		name   string
		down   string
		blip   string
		args   []string
		checks map[string]int
	}{
		{name: "named run, a blip where it is", blip: "b", args: []string{"pg-main-earlier"}, checks: map[string]int{"a": 1, "b": 2}},
		{name: "named run, found, a site down", down: "a", args: []string{"pg-main-earlier"}, checks: map[string]int{"a": 1, "b": 1}},
		{name: "every run, a site down", down: "a", checks: map[string]int{"a": 3, "b": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFactory(objects())
			if tt.down != "" {
				f.clients.SetDown(tt.down, true)
			}
			if tt.blip != "" {
				f.clients.Blip(tt.blip, 1)
			}
			out, code := run(t, f, append([]string{"status", "-f", "testdata/workload.yaml"}, tt.args...)...)
			if code != ExitOK || !strings.Contains(out, "pg-main-earlier") {
				t.Fatalf("exit %d, want %d with the run:\n%s", code, ExitOK, out)
			}
			for site, n := range tt.checks {
				if got := f.clients.Checks[site]; got != n {
					t.Errorf("site %s checked %d times, want %d", site, got, n)
				}
			}
		})
	}
}

func TestSkill(t *testing.T) {
	out, code := run(t, newFactory(nil), "--skill")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.HasPrefix(out, "---\nname: tack\ndescription: ") {
		t.Fatalf("no skill frontmatter:\n%s", out)
	}
	for _, want := range []string{
		`An agent never starts "tack run" or "tack resume" without an explicit`,
		`"tack plan" comes first`,
		"### tack plan", "### tack run", "### tack resume", "### tack status",
		"`--accept-git-head`", "`--fence`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("skill lacks %q", want)
		}
	}
	golden(t, "skill", out)
}

func TestRunNeedsConfirmation(t *testing.T) {
	isTerminal = func() bool { return false }
	defer func() { isTerminal = stdinIsTerminal }()
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b")
	if code != ExitUsage || !strings.Contains(out, "pass --yes once a human approved this run") {
		t.Fatalf("exit %d, want %d with a refusal:\n%s", code, ExitUsage, out)
	}
	if len(f.flux.Held) != 0 {
		t.Fatal("run held objects without confirmation")
	}
}

func TestRunWrongConfirmation(t *testing.T) {
	isTerminal = func() bool { return true }
	stdin = strings.NewReader("yes\n")
	defer func() { isTerminal, stdin = stdinIsTerminal, os.Stdin }()
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b")
	if code != ExitError || !strings.Contains(out, "not confirmed; nothing was changed") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestPlanDeprecatedWorkload(t *testing.T) {
	b, err := os.ReadFile("testdata/workload.yaml")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "workload.yaml")
	old := strings.Replace(string(b), "apiVersion: failover.tako.id/v1\n", "apiVersion: failover.tako.id/v1alpha1\n", 1)
	if err := os.WriteFile(file, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	out, code := run(t, f, "plan", "-f", file, "--to", "b")
	warning := "tack: warning: " + file + ": apiVersion failover.tako.id/v1alpha1 is deprecated; use failover.tako.id/v1\n"
	if code != ExitOK || !strings.HasPrefix(out, warning) || !strings.Contains(out, "Nothing was changed.") {
		t.Fatalf("exit %d, want %d with the warning and the plan:\n%s", code, ExitOK, out)
	}
}

// A site whose check fails with a refused credential is not lost: the
// search for runs fails instead of leaving its runs out. Issue #25.
func TestSearchFailsOnRefusedCredential(t *testing.T) {
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "b", 0),
		"b": append(testenv.Site("b", "b", 2), siteLossRun("a", fixedNow.Add(-time.Hour))),
	})
	f.clients.Fail("b", apierrors.NewUnauthorized("Unauthorized"))
	out, code := run(t, f, "status", "-f", "testdata/workload.yaml")
	if code != ExitError || !strings.Contains(out, "site b: search for runs") {
		t.Fatalf("exit %d, want %d naming site b:\n%s", code, ExitError, out)
	}
	if n := f.clients.Checks["b"]; n != 1 {
		t.Errorf("site b checked %d times, want 1: a refused credential is not repeated", n)
	}
}

// stoppedRun is a run of testdata/workload.yaml from a to b that stopped,
// with its ledger at site b.
func stoppedRun(t *testing.T, name string) *failoverv1.SiteFailover {
	t.Helper()
	w, err := config.Load("testdata/workload.yaml")
	if err != nil {
		t.Fatal(err)
	}
	started := metav1.NewTime(fixedNow.Add(-time.Hour))
	return &failoverv1.SiteFailover{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tack-system", Labels: map[string]string{engine.LabelWorkload: "pg-main"},
			CreationTimestamp: started},
		Spec: failoverv1.SiteFailoverSpec{
			Workload: failoverv1.WorkloadSnapshot{Name: w.Name, Spec: w.Spec}, Mode: failoverv1.ModePlanned, From: "a", To: "b",
			Steps: []failoverv1.Step{{ID: "hold-a-cluster-pg-a", Phase: failoverv1.PhaseHold, Type: failoverv1.StepHold, Site: "a",
				Hold: &failoverv1.ObjectStep{Object: failoverv1.ObjectRef{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Namespace: "db", Name: "pg-a"}}}},
		},
		Status: failoverv1.SiteFailoverStatus{State: failoverv1.RunStopped, Reason: failoverv1.ReasonStepFailed, StartedAt: &started,
			Steps: []failoverv1.StepStatus{{ID: "hold-a-cluster-pg-a", State: failoverv1.StepFailed}}},
	}
}

// tack resume with --context finds the run at one site, but searches every
// site of the run's Workload snapshot for a newer run. Issue #27.
func TestResumeContextSearchesEverySite(t *testing.T) {
	older := stoppedRun(t, "pg-main-older")
	newer := siteLossRun("a", fixedNow)
	newer.Name, newer.CreationTimestamp = "pg-main-newer", metav1.NewTime(fixedNow)
	f := newFactory(map[string][]client.Object{
		"a": append(testenv.Site("a", "b", 0), newer),
		"b": append(testenv.Site("b", "b", 2), older),
	})
	// The fakes ignore contexts: the context's site is site b's cluster.
	f.clients.Sites[contextSite] = f.clients.Sites["b"]
	out, code := run(t, f, "resume", "pg-main-older", "--context", "site-b", "--yes")
	if code != ExitError || !strings.Contains(out, "run pg-main-newer started after run pg-main-older") {
		t.Fatalf("exit %d, want %d with a refusal naming the newer run at site a:\n%s", code, ExitError, out)
	}
	if len(f.flux.Calls) > 0 {
		t.Errorf("the refused resume changed something: %v", f.flux.Calls)
	}
}

// A run started with --take-over records the run it took over, and marks
// that run's ledger. The old run then cannot resume, and no longer blocks
// a new run. Issue #27.
func TestTakeOverIsRecorded(t *testing.T) {
	f := newFactory(map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), stoppedRun(t, "pg-main-earlier")),
	})
	f.flux.Fail["Hold"] = errors.New("stop at the first hold")
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--take-over", "pg-main-earlier", "--yes")
	if code != ExitStopped {
		t.Fatalf("run exit %d, want %d:\n%s", code, ExitStopped, out)
	}
	store := &ledger.Store{Client: f.clients.Sites["b"], Namespace: "tack-system"}
	ctx := context.Background()
	if sf, err := store.Get(ctx, "pg-main-20261027-101500"); err != nil || sf.Spec.TakeOver != "pg-main-earlier" {
		t.Fatalf("new run: %v, spec.takeOver %q", err, sf.Spec.TakeOver)
	}
	if sf, err := store.Get(ctx, "pg-main-earlier"); err != nil || sf.Status.SupersededBy != "pg-main-20261027-101500" {
		t.Fatalf("old run: %v, status.supersededBy %q", err, sf.Status.SupersededBy)
	}
	delete(f.flux.Fail, "Hold")
	calls := len(f.flux.Calls)

	out, code = run(t, f, "resume", "pg-main-earlier", "-f", "testdata/workload.yaml", "--yes")
	if code != ExitError || !strings.Contains(out, "run pg-main-earlier was taken over by run pg-main-20261027-101500") {
		t.Fatalf("resume of the old run: exit %d, want %d with a refusal:\n%s", code, ExitError, out)
	}
	out, code = run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--yes")
	if code != ExitError || !strings.Contains(out, "run pg-main-20261027-101500 at site b is Stopped") || strings.Contains(out, "run pg-main-earlier at site") {
		t.Fatalf("a third run: exit %d, want %d naming only the new run:\n%s", code, ExitError, out)
	}
	if len(f.flux.Calls) != calls {
		t.Errorf("the refused commands changed something: %v", f.flux.Calls[calls:])
	}
}

// Issue #37. A run that takes over a run planned in the same second gets
// the next free name: the run it takes over never recorded its start, and
// holds the name. The e2e's TestTakeOver hits this on a fast machine.
func TestTakeOverInTheSameSecond(t *testing.T) {
	never := stoppedRun(t, "pg-main-20261027-101500")
	never.Status = failoverv1.SiteFailoverStatus{}
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": append(testenv.Site("b", "a", 0), never)})
	f.flux.Fail["Hold"] = errors.New("stop at the first hold")
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--take-over", "pg-main-20261027-101500", "--yes")
	if code != ExitStopped || !strings.Contains(out, "this run is named pg-main-20261027-101500-2\n") ||
		!strings.Contains(out, "ledger tack-system/pg-main-20261027-101500-2 created") {
		t.Fatalf("exit %d, want %d with a run named pg-main-20261027-101500-2:\n%s", code, ExitStopped, out)
	}
	store := &ledger.Store{Client: f.clients.Sites["b"], Namespace: "tack-system"}
	ctx := context.Background()
	if sf, err := store.Get(ctx, "pg-main-20261027-101500-2"); err != nil || sf.Spec.TakeOver != "pg-main-20261027-101500" {
		t.Fatalf("new run: %v, spec.takeOver %q", err, sf.Spec.TakeOver)
	}
	if sf, err := store.Get(ctx, "pg-main-20261027-101500"); err != nil || sf.Status.SupersededBy != "pg-main-20261027-101500-2" {
		t.Fatalf("old run: %v, status.supersededBy %q", err, sf.Status.SupersededBy)
	}
}

// A run that never recorded its start still counts for the check of a
// member left to rebuild: its ledger's creation stands in for its start.
// Issue #28.
func TestPlanRebuildCheckWithoutStart(t *testing.T) {
	earlier := siteLossRun("a", fixedNow.Add(-time.Hour))
	earlier.CreationTimestamp, earlier.Status.StartedAt = metav1.NewTime(fixedNow.Add(-time.Hour)), nil
	f := newFactory(map[string][]client.Object{
		"a": {created(testenv.Cluster("a", "b", false, nil), fixedNow.Add(-2*time.Hour)), testenv.Pooler("a", 0)},
		"b": {testenv.Cluster("b", "b", true, nil), testenv.Pooler("b", 2), earlier},
	})
	out, code := run(t, f, "plan", "-f", "testdata/workload.yaml", "--to", "a")
	if code != ExitError || !strings.Contains(out, "rebuild it before a planned switchover") {
		t.Fatalf("exit %d, want %d with the rebuild refusal:\n%s", code, ExitError, out)
	}
}

// A ledger that never recorded its start cannot resume, and a new run is
// told to take it over. Issue #28.
func TestRunNeverStarted(t *testing.T) {
	never := stoppedRun(t, "pg-main-earlier")
	never.Status = failoverv1.SiteFailoverStatus{}
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": append(testenv.Site("b", "a", 0), never)})
	out, code := run(t, f, "resume", "pg-main-earlier", "-f", "testdata/workload.yaml", "--yes")
	if code != ExitError || !strings.Contains(out, "never recorded its start") || !strings.Contains(out, "--take-over pg-main-earlier") {
		t.Fatalf("resume: exit %d, want %d with a refusal:\n%s", code, ExitError, out)
	}
	out, code = run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b", "--yes")
	if code != ExitError || !strings.Contains(out, "run pg-main-earlier at site b never recorded its start") {
		t.Fatalf("run: exit %d, want %d naming the run:\n%s", code, ExitError, out)
	}
	if len(f.flux.Calls) > 0 {
		t.Errorf("something changed: %v", f.flux.Calls)
	}
}

// confirmAfter types the confirmation once f has run, as an operator does
// after a while.
type confirmAfter struct {
	f    func()
	line io.Reader
}

func (c *confirmAfter) Read(p []byte) (int, error) {
	if c.f != nil {
		c.f()
		c.f = nil
	}
	return c.line.Read(p)
}

// typeAfter makes the next confirmation read "pg-main" after f ran.
func typeAfter(t *testing.T, f func()) {
	t.Helper()
	isTerminal = func() bool { return true }
	stdin = &confirmAfter{f: f, line: strings.NewReader("pg-main\n")}
	t.Cleanup(func() { isTerminal, stdin = stdinIsTerminal, os.Stdin })
}

func mergePatch(t *testing.T, c client.Client, kind, name, patch string) {
	t.Helper()
	u := testenv.Object("postgresql.cnpg.io/v1", kind, "db", name, nil)
	if err := c.Patch(context.Background(), u, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatal(err)
	}
}

// Issue #30. While the operator confirms a planned switchover from a to b,
// another run makes the same switch and succeeds. The confirmed plan must
// not run: it would capture a's leftover demotion token and write a used
// token back to b.
func TestRunRefusesPlanThatWentStale(t *testing.T) {
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	// A run that starts stops at its first hold, which Flux records.
	f.flux.Fail["Hold"] = errors.New("stop at the first hold")
	typeAfter(t, func() {
		mergePatch(t, f.clients.Sites["a"], "Cluster", "pg-a",
			`{"spec":{"replica":{"primary":"pg-b"}},"status":{"demotionToken":"token-used","instancesReportedState":{"pg-a-1":{"isPrimary":false}}}}`)
		mergePatch(t, f.clients.Sites["b"], "Cluster", "pg-b",
			`{"spec":{"replica":{"primary":"pg-b"}},"status":{"lastPromotionToken":"token-used","instancesReportedState":{"pg-b-1":{"isPrimary":true}}}}`)
		done := siteLossRun("a", fixedNow)
		done.Name, done.Spec.Mode, done.Status.FollowUps = "pg-main-other", failoverv1.ModePlanned, nil
		if err := f.clients.Sites["b"].Create(context.Background(), done); err != nil {
			t.Fatal(err)
		}
	})
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b")
	if code == ExitOK || len(f.flux.Calls) > 0 {
		t.Fatalf("the stale plan ran: exit %d, Flux %v\n%s", code, f.flux.Calls, out)
	}
	var l failoverv1.SiteFailoverList
	if err := f.clients.Sites["b"].List(context.Background(), &l); err != nil || len(l.Items) != 1 {
		t.Errorf("%d ledgers at site b, want only the other run's: %v", len(l.Items), err)
	}
}

// A run of the workload that started after the plan, at a site the plan
// searched, refuses the plan even when the sites look the same.
func TestRunRefusesPlanAfterAnotherRun(t *testing.T) {
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	f.flux.Fail["Hold"] = errors.New("stop at the first hold")
	typeAfter(t, func() {
		other := siteLossRun("a", fixedNow)
		other.Name, other.Spec.Mode, other.Status.FollowUps = "pg-main-other", failoverv1.ModePlanned, nil
		if err := f.clients.Sites["a"].Create(context.Background(), other); err != nil {
			t.Fatal(err)
		}
	})
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b")
	if code != ExitError || !strings.Contains(out, "run pg-main-other of the workload started after the plan was made") {
		t.Fatalf("exit %d, want %d with a refusal:\n%s", code, ExitError, out)
	}
	if len(f.flux.Calls) > 0 {
		t.Errorf("the plan ran: %v", f.flux.Calls)
	}
}

// A site that was lost at plan time answers again while the operator
// confirms, with older ledgers. The plan did not search it, so they do not
// make it stale.
func TestRunLostSiteBackAtConfirmation(t *testing.T) {
	older := siteLossRun("b", fixedNow.Add(-24*time.Hour))
	older.Name, older.Spec.To = "pg-main-older", "a"
	f := newFactory(map[string][]client.Object{
		"a": append(testenv.Site("a", "b", 0), older),
		"b": testenv.Site("b", "b", 2),
	}, "b")
	f.flux.Fail["Hold"] = errors.New("stop at the first hold")
	typeAfter(t, func() { f.clients.SetDown("b", false) })
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "a", "--mode", "site-loss")
	if code != ExitStopped || !strings.Contains(out, "stop at the first hold") {
		t.Fatalf("exit %d, want %d at the first hold:\n%s", code, ExitStopped, out)
	}
}

// A context that reaches another cluster after the plan refuses it, even
// when that cluster holds the same objects.
func TestRunRefusesPlanOnAnotherCluster(t *testing.T) {
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	other := testenv.NewClients(map[string][]client.Object{"x": testenv.Site("a", "a", 2)})
	typeAfter(t, func() { f.clients.SetSite("a", other.Sites["x"]) })
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b")
	if code != ExitError || !strings.Contains(out, "site a now reaches another cluster: kube-system UID uid-x, the plan read uid-a") {
		t.Fatalf("exit %d, want %d with a refusal:\n%s", code, ExitError, out)
	}
}

// A ledger written before Tack refused a credential in the Git URL prints
// it redacted in YAML. Issue #31.
func TestYAMLRedactsGitURL(t *testing.T) {
	old := stoppedRun(t, "pg-main-earlier")
	old.Spec.Workload.Spec.Git.URL = "https://user:FAKE-TOKEN-123@git.example.com/gitops.git"
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": append(testenv.Site("b", "a", 0), old)})
	for _, args := range [][]string{
		{"status", "-f", "testdata/workload.yaml", "-o", "yaml"},
		{"status", "pg-main-earlier", "-f", "testdata/workload.yaml", "-o", "yaml"},
	} {
		out, code := run(t, f, args...)
		if code != ExitOK || strings.Contains(out, "FAKE-TOKEN-123") || !strings.Contains(out, "https://***@git.example.com/gitops.git") {
			t.Errorf("%v: exit %d, the URL is not redacted:\n%s", args, code, out)
		}
	}
	if out, _ := run(t, f, "status", "pg-main-earlier", "-f", "testdata/workload.yaml"); strings.Contains(out, "FAKE-TOKEN-123") {
		t.Errorf("tack status prints the credential:\n%s", out)
	}
}
