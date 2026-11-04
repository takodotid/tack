package engine_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/config"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/driver/cnpg"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/testenv"
)

const workloadYAML = `apiVersion: failover.tako.id/v1
kind: Workload
metadata: {name: pg-main}
spec:
  driver: cnpg
  sites:
  - {name: a, kubeContext: site-a, cnpg: {cluster: {namespace: db, name: pg-a}, pooler: {namespace: db, name: pg-a-pooler-rw, instances: 2}}}
  - {name: b, kubeContext: site-b, cnpg: {cluster: {namespace: db, name: pg-b}, pooler: {namespace: db, name: pg-b-pooler-rw, instances: 2}}}
  git: {url: https://git.example.com/gitops.git, branch: main, find: {}}
  timeouts: {demotion: 5s, promotion: 5s, flux: 5s}
`

type fixture struct {
	t       *testing.T
	clients *testenv.Clients
	flux    *testenv.Flux
	git     *testenv.Git
	w       *config.Workload
	cancel  context.CancelFunc
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	w, err := config.Parse([]byte(workloadYAML))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t:       t,
		clients: testenv.NewClients(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)}),
		flux:    testenv.NewFlux("main@sha1:1111111111111111111111111111111111111111"),
		git:     &testenv.Git{},
		w:       w,
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go (&testenv.Operator{Clients: f.clients, Sites: []string{"a", "b"}}).Run(ctx)
	t.Cleanup(cancel)
	return f
}

func (f *fixture) env(ledgerSite string) *engine.Env {
	cl := f.clients.Sites[ledgerSite]
	return &engine.Env{
		Clients: f.clients, Driver: cnpg.Driver{}, Flux: f.flux, Git: f.git,
		Ledger: &ledger.Store{Client: cl, Namespace: "tack-system"},
		Out:    io.Discard, Poll: time.Millisecond,
		ReachWaits: []time.Duration{time.Millisecond, time.Millisecond},
	}
}

func (f *fixture) plan(to string, mode failoverv1.Mode) *failoverv1.SiteFailover {
	f.t.Helper()
	from := "a"
	if to == "a" {
		from = "b"
	}
	p, err := f.env(to).MakePlan(context.Background(), engine.Request{
		Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
		Request: driver.Request{From: from, To: to, Mode: mode},
	})
	if err != nil {
		f.t.Fatalf("plan: %v", err)
	}
	return p.SiteFailover
}

func (f *fixture) start(to string, mode failoverv1.Mode, opts engine.Options) (*failoverv1.SiteFailover, error) {
	f.t.Helper()
	return f.run(to, f.plan(to, mode), opts)
}

func (f *fixture) run(to string, sf *failoverv1.SiteFailover, opts engine.Options) (*failoverv1.SiteFailover, error) {
	f.t.Helper()
	err := f.env(to).Start(context.Background(), sf, opts)
	return f.ledger(to), err
}

func (f *fixture) resume(site string, opts engine.Options) (*failoverv1.SiteFailover, error) {
	f.t.Helper()
	env := f.env(site)
	sf := f.ledger(site)
	err := env.Resume(context.Background(), sf, opts)
	return f.ledger(site), err
}

func (f *fixture) ledger(site string) *failoverv1.SiteFailover {
	f.t.Helper()
	sf, err := f.env(site).Ledger.Get(context.Background(), "run-1")
	if err != nil {
		f.t.Fatalf("read ledger: %v", err)
	}
	return sf
}

func (f *fixture) cluster(site string) map[string]any {
	f.t.Helper()
	u := testenv.Object("postgresql.cnpg.io/v1", "Cluster", "db", "pg-"+site, nil)
	if err := f.clients.Sites[site].Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
		f.t.Fatal(err)
	}
	return u.Object
}

func field(obj map[string]any, path ...string) any {
	v, _, _ := unstructured.NestedFieldNoCopy(obj, path...)
	return v
}

func stepState(sf *failoverv1.SiteFailover, id string) failoverv1.StepState {
	for _, s := range sf.Status.Steps {
		if s.ID == id {
			return s.State
		}
	}
	return ""
}

func change(req engine.CommitRequest, site, f string) (string, bool) {
	for _, c := range req.Changes {
		if c.Site == site && c.Field == f {
			if c.Value == nil {
				return "", true
			}
			return *c.Value, true
		}
	}
	return "", false
}

func TestPlannedRun(t *testing.T) {
	f := newFixture(t)
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, sf.Status.Message)
	}
	if sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("state %s: %s", sf.Status.State, sf.Status.Message)
	}
	tok := sf.Status.Captured[cnpg.TokenKey]
	if !strings.HasPrefix(tok, "token-a-") {
		t.Fatalf("captured token %q", tok)
	}
	b := f.cluster("b")
	if field(b, "spec", "replica", "primary") != "pg-b" || field(b, "spec", "replica", "promotionToken") != nil {
		t.Fatalf("site b spec: %v", field(b, "spec", "replica"))
	}
	if field(f.cluster("a"), "spec", "replica", "primary") != "pg-b" {
		t.Fatal("site a still declares itself primary")
	}
	if len(f.git.Commits) != 1 {
		t.Fatalf("%d commits", len(f.git.Commits))
	}
	req := f.git.Commits[0]
	if req.Base != "1111111111111111111111111111111111111111" {
		t.Errorf("commit base %q", req.Base)
	}
	if v, _ := change(req, "a", cnpg.FieldPrimary); v != `"pg-b"` {
		t.Errorf("Git primary at a = %s", v)
	}
	if v, ok := change(req, "b", cnpg.FieldToken); !ok || v != "" {
		t.Errorf("Git must remove the token at b, got %q, %v", v, ok)
	}
	for _, c := range req.Changes {
		if c.Value != nil && strings.Contains(*c.Value, "token-") {
			t.Errorf("a token reached Git: %s", *c.Value)
		}
	}
	if len(f.flux.Held) != 0 {
		t.Errorf("objects still held: %v", f.flux.Held)
	}
	for _, c := range calls(f.flux, "Reconcile") {
		if !strings.Contains(c, " fetch ") {
			t.Errorf("a planned run reconciled without a Git fetch: %q", c)
		}
	}
	if f.clients.Writes == 0 {
		t.Error("the fake clients counted no write")
	}
	if len(sf.Status.Phases) != 4 {
		t.Errorf("phases %v", sf.Status.Phases)
	}
	for _, p := range sf.Status.Phases {
		if p.StartedAt == nil || p.FinishedAt == nil {
			t.Errorf("phase %s not recorded: %+v", p.Name, p)
		}
	}
}

func TestResumeAfterStop(t *testing.T) {
	f := newFixture(t)
	f.flux.Fail["VerifyGit"] = errors.New("render failed")
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("err %v, want stopped", err)
	}
	if sf.Status.State != failoverv1.RunStopped || sf.Status.Reason != failoverv1.ReasonStepFailed || stepState(sf, "verify-git-a") != failoverv1.StepFailed {
		t.Fatalf("state %s %s %s", sf.Status.State, sf.Status.Reason, stepState(sf, "verify-git-a"))
	}
	holds := countCalls(f.flux, "Hold")
	delete(f.flux.Fail, "VerifyGit")
	sf, err = f.resume("b", engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s", err, sf.Status.Message)
	}
	if countCalls(f.flux, "Hold") != holds {
		t.Error("resume ran Hold steps again")
	}
	if len(f.git.Commits) != 1 {
		t.Errorf("%d commits after resume", len(f.git.Commits))
	}
}

func TestResumeReRunsStartedStep(t *testing.T) {
	f := newFixture(t)
	f.flux.Fail["VerifyGit"] = errors.New("stop here")
	sf, _ := f.start("b", failoverv1.ModePlanned, engine.Options{})
	// A process that died after starting a step leaves it Running.
	for i := range sf.Status.Steps {
		if sf.Status.Steps[i].ID == "reconcile-git-b" {
			sf.Status.Steps[i].State = failoverv1.StepRunning
		}
	}
	if err := f.env("b").Ledger.UpdateStatus(context.Background(), sf); err != nil {
		t.Fatal(err)
	}
	delete(f.flux.Fail, "VerifyGit")
	before := countCalls(f.flux, "Reconcile")
	sf, err := f.resume("b", engine.Options{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if stepState(sf, "reconcile-git-b") != failoverv1.StepSucceeded || countCalls(f.flux, "Reconcile") <= before {
		t.Fatal("the started step did not run again")
	}
}

func TestGitChangedDuringRun(t *testing.T) {
	f := newFixture(t)
	other := "2222222222222222222222222222222222222222"
	f.git.Head = other
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged {
		t.Fatalf("err %v, reason %s", err, sf.Status.Reason)
	}
	if stepState(sf, "git-commit") != failoverv1.StepFailed || stepState(sf, "verify-b") != failoverv1.StepSucceeded {
		t.Fatal("the run should switch live, then stop at the Git step")
	}
	sf, err = f.resume("b", engine.Options{AcceptGitHead: other})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s", err, sf.Status.Message)
	}
	if f.git.Commits[0].Base != other || sf.Status.Git.AcceptedRevision != other {
		t.Fatalf("base %s, accepted %s", f.git.Commits[0].Base, sf.Status.Git.AcceptedRevision)
	}
}

func TestSiteLossBreakGlassAndReturn(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	f.git.Unreachable = true
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if !errors.Is(err, engine.ErrWaiting) || sf.Status.Reason != failoverv1.ReasonGitUnreachable {
		t.Fatalf("err %v, state %s %s", err, sf.Status.State, sf.Status.Reason)
	}
	if field(f.cluster("b"), "status", "instancesReportedState", "pg-b-1", "isPrimary") != true {
		t.Fatal("site b was not promoted during break-glass")
	}
	if stepState(sf, "fence-a") != failoverv1.StepDeferred || stepState(sf, "hold-a-cluster-pg-a") != failoverv1.StepDeferred {
		t.Fatal("site a's steps were not deferred")
	}
	if len(sf.Status.FollowUps) != 1 || sf.Status.FollowUps[0].Action != "Rebuild" {
		t.Fatalf("follow-ups %v", sf.Status.FollowUps)
	}

	// Site a comes back, still primary, while Git is still down.
	f.clients.SetDown("a", false)
	sf, err = f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary {
		t.Fatalf("err %v, reason %s", err, sf.Status.Reason)
	}
	if field(f.cluster("a"), "spec", "replica", "primary") != "pg-a" {
		t.Fatal("site a was changed without approval")
	}

	sf, err = f.resume("b", engine.Options{Fence: []string{"a"}})
	if !errors.Is(err, engine.ErrWaiting) {
		t.Fatalf("err %v, state %s %s", err, sf.Status.State, sf.Status.Reason)
	}
	if field(f.cluster("a"), "spec", "replica", "primary") != "pg-b" {
		t.Fatal("site a was not fenced")
	}
	if stepState(sf, "hold-a-cluster-pg-a") != failoverv1.StepSucceeded {
		t.Fatal("site a's objects were not held before the fence")
	}

	f.git.Unreachable = false
	sf, err = f.resume("b", engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	for _, st := range sf.Status.Steps {
		if st.State != failoverv1.StepSucceeded {
			t.Errorf("step %s is %s", st.ID, st.State)
		}
	}
	if len(f.flux.Held) != 0 {
		t.Errorf("objects still held: %v", f.flux.Held)
	}
}

// A site loss with Git cut: a GitRepository asked to fetch hangs. Hold
// must not ask it, so the run promotes without waiting on Git.
func TestSiteLossHoldsWithoutGit(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	f.git.Unreachable = true
	f.flux.GitDown = true // a fetch hangs for 2.5s, half the Flux timeout
	start := time.Now()
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if !errors.Is(err, engine.ErrWaiting) || sf.Status.Reason != failoverv1.ReasonGitUnreachable {
		t.Fatalf("err %v, state %s %s", err, sf.Status.State, sf.Status.Reason)
	}
	if took := time.Since(start); took >= 2500*time.Millisecond {
		t.Errorf("the run took %s to reach the Git step: it waited on a Git fetch", took)
	}
	if field(f.cluster("b"), "status", "instancesReportedState", "pg-b-1", "isPrimary") != true {
		t.Fatal("site b was not promoted")
	}
	if got := calls(f.flux, "Reconcile"); strings.Join(got, ",") != "Reconcile b no-fetch " {
		t.Errorf("Reconcile calls %q, want one at b without a Git fetch", got)
	}
	if msg := stepMessage(sf, "reconcile-hold-b"); !strings.Contains(msg, "Git not fetched") {
		t.Errorf("reconcile-hold-b message %q", msg)
	}

	// Git is back: the Git step and the later reconciles fetch it.
	f.git.Unreachable, f.flux.GitDown = false, false
	sf, err = f.resume("b", engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	got := calls(f.flux, "Reconcile")
	if len(got) != 3 || !strings.HasPrefix(got[1], "Reconcile b fetch c") || !strings.HasPrefix(got[2], "Reconcile b fetch c") {
		t.Errorf("Reconcile calls %q: the Git and Release phases must fetch the run's commit", got)
	}
}

// A short network cut between the operator's host and a site fails a
// check or two of the site's API. The run must neither stop nor defer.
func TestSiteCheckRetried(t *testing.T) {
	f := newFixture(t)
	plan := f.plan("b", failoverv1.ModePlanned)
	f.clients.Blip("a", 2)
	before := f.clients.Checks["a"]
	sf, err := f.run("b", plan, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	if n := f.clients.Checks["a"] - before; n != 3 {
		t.Errorf("site a checked %d times, want 3", n)
	}
	if msg := stepMessage(sf, "hold-a-cluster-pg-a"); !strings.Contains(msg, "site a answered on check 3 of 3") {
		t.Errorf("hold-a-cluster-pg-a message %q", msg)
	}
}

// A site that answers only on a later check, then fails its first step:
// the step's message still says how many checks ran.
func TestSiteCheckRetriedThenStepFails(t *testing.T) {
	f := newFixture(t)
	plan := f.plan("b", failoverv1.ModePlanned)
	f.clients.Blip("a", 1)
	f.flux.Fail["Hold"] = errors.New("hold failed")
	sf, err := f.run("b", plan, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("err %v, want stopped", err)
	}
	if msg := stepMessage(sf, "hold-a-cluster-pg-a"); !strings.Contains(msg, "hold failed") || !strings.Contains(msg, "site a answered on check 2 of 3") {
		t.Errorf("hold-a-cluster-pg-a message %q", msg)
	}
}

// A site that stays down counts as unreachable after a bounded number of
// checks. The to site then stops the run.
func TestSiteCheckBounded(t *testing.T) {
	f := newFixture(t)
	plan := f.plan("b", failoverv1.ModePlanned)
	f.clients.SetDown("b", true)
	before := f.clients.Checks["b"]
	sf, err := f.run("b", plan, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonStepFailed {
		t.Fatalf("err %v, state %s %s", err, sf.Status.State, sf.Status.Reason)
	}
	if n := f.clients.Checks["b"] - before; n != 3 {
		t.Errorf("site b checked %d times, want 3", n)
	}
	if st := stepState(sf, "hold-b-cluster-pg-b"); st != failoverv1.StepFailed {
		t.Errorf("hold-b-cluster-pg-b is %s", st)
	}
	if !strings.Contains(sf.Status.Message, "no answer to 3 checks") {
		t.Errorf("message %q", sf.Status.Message)
	}
}

func TestSiteLossSiteStaysDown(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	plan := f.plan("b", failoverv1.ModeSiteLoss)
	before := f.clients.Checks["a"]
	sf, err := f.run("b", plan, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %v %s", err, sf.Status.Message)
	}
	// The run does not wait for a site it can do without.
	if n := f.clients.Checks["a"] - before; n != 1 {
		t.Errorf("lost site a checked %d times, want 1", n)
	}
	if msg := stepMessage(sf, "fence-a"); !strings.Contains(msg, "site a unreachable: ") {
		t.Errorf("fence-a message %q", msg)
	}
	if !strings.Contains(sf.Status.Message, "wait for their site") {
		t.Errorf("message %q", sf.Status.Message)
	}
	if v, _ := change(f.git.Commits[0], "a", cnpg.FieldPrimary); v != `"pg-b"` {
		t.Errorf("Git does not demote site a: %s", v)
	}
}

// A lost site answers the check before its deferred steps, then fails
// every check of the read of the fence check. Its steps must stay
// deferred: an unread site is not known to be fenced.
func TestFenceCheckCannotRead(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %v %s", err, sf.Status.Message)
	}

	// Site a comes back, still primary, and flaps.
	f.clients.SetDown("a", false)
	f.clients.Flap("a", true, false, false, false)
	sf, err = f.resume("b", engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	if field(f.cluster("a"), "spec", "replica", "primary") != "pg-a" {
		t.Fatal("site a was fenced although the fence check could not read it")
	}
	for _, id := range []string{"hold-a-cluster-pg-a", "fence-a"} {
		if st := stepState(sf, id); st != failoverv1.StepDeferred {
			t.Errorf("%s is %s, want Deferred", id, st)
		}
	}
	if msg := stepMessage(sf, "hold-a-cluster-pg-a"); !strings.Contains(msg, "did not answer the read") || !strings.Contains(msg, "no answer to 3 checks") {
		t.Errorf("hold-a-cluster-pg-a message %q", msg)
	}

	// Read, it answers as primary: the run stops for the operator.
	sf, err = f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary {
		t.Fatalf("err %v, reason %s", err, sf.Status.Reason)
	}
}

// A short network cut fails a check or two of the to site's API. The plan
// must still read the site, and note the checks.
func TestPlanSiteCheckRetried(t *testing.T) {
	f := newFixture(t)
	f.clients.Blip("b", 2)
	p, err := f.env("b").MakePlan(context.Background(), engine.Request{
		Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
		Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if n := f.clients.Checks["b"]; n != 3 {
		t.Errorf("site b checked %d times, want 3", n)
	}
	if !slices.Contains(p.Notes, "site b answered on check 3 of 3") {
		t.Errorf("notes %q", p.Notes)
	}
}

// A planned plan needs from. A from site that stays down is refused after
// the bounded checks.
func TestPlanSiteCheckBounded(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	_, err := f.env("b").MakePlan(context.Background(), engine.Request{
		Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
		Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned},
	})
	if err == nil || !strings.Contains(err.Error(), "a planned switchover needs it") {
		t.Fatalf("err %v, want a refusal", err)
	}
	if n := f.clients.Checks["a"]; n != 3 {
		t.Errorf("site a checked %d times, want 3", n)
	}
}

// A site-loss plan checks the lost site once: it does not answer, and more
// checks would add to the write outage.
func TestPlanSiteLossChecksLostSiteOnce(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	p, err := f.env("b").MakePlan(context.Background(), engine.Request{
		Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
		Request: driver.Request{To: "b", Mode: failoverv1.ModeSiteLoss},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if n := f.clients.Checks["a"]; n != 1 {
		t.Errorf("lost site a checked %d times, want 1", n)
	}
	if p.SiteFailover.Spec.From != "a" {
		t.Errorf("from %q, want a, the primary site b declares", p.SiteFailover.Spec.From)
	}
}

// A lost site comes back and fails the first check of the read before its
// deferred steps. It has just answered the check before that step, so the
// read checks it again, and its steps run.
func TestFenceCheckRetried(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %v %s", err, sf.Status.Message)
	}

	f.clients.SetDown("a", false)
	f.clients.Flap("a", true, false)
	sf, err = f.resume("b", engine.Options{Fence: []string{"a"}})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	for _, id := range []string{"hold-a-cluster-pg-a", "fence-a"} {
		if st := stepState(sf, id); st != failoverv1.StepSucceeded {
			t.Errorf("%s is %s, want Succeeded", id, st)
		}
	}
	if field(f.cluster("a"), "spec", "replica", "primary") != "pg-b" {
		t.Error("site a was not fenced")
	}
	if msg := stepMessage(sf, "hold-a-cluster-pg-a"); !strings.Contains(msg, "the read before its deferred steps: site a answered on check 2 of 3") {
		t.Errorf("hold-a-cluster-pg-a message %q", msg)
	}
}

func TestHeldByAnotherRun(t *testing.T) {
	f := newFixture(t)
	f.flux.Held["a/Cluster/pg-a"] = "run-0"
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonHeldByAnotherRun {
		t.Fatalf("err %v, reason %s", err, sf.Status.Reason)
	}
	if field(f.cluster("a"), "spec", "replica", "primary") != "pg-a" {
		t.Fatal("the run changed live objects without holding them")
	}
}

func calls(f *testenv.Flux, name string) []string {
	var out []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, name+" ") {
			out = append(out, c)
		}
	}
	return out
}

func stepMessage(sf *failoverv1.SiteFailover, id string) string {
	for _, s := range sf.Status.Steps {
		if s.ID == id {
			return s.Message
		}
	}
	return ""
}

func countCalls(f *testenv.Flux, name string) int {
	n := 0
	for _, c := range f.Calls {
		if strings.HasPrefix(c, name+" ") {
			n++
		}
	}
	return n
}
