package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
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
  timeouts: {demotion: 5s, promotion: 5s, flux: 5s, snapshot: 5s}
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
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if !errors.Is(err, engine.ErrWaiting) || sf.Status.Reason != failoverv1.ReasonGitUnreachable {
		t.Fatalf("err %v, state %s %s", err, sf.Status.State, sf.Status.Reason)
	}
	// A count, not the run's time: the race detector makes the run itself
	// take longer than a fetch hangs (#58).
	if f.flux.Hung != 0 {
		t.Errorf("the run waited on %d Git fetches before the Git step", f.flux.Hung)
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
	if msg := stepMessage(sf, "hold-a-cluster-pg-a"); !strings.Contains(msg, "the read before its steps: site a answered on check 2 of 3") {
		t.Errorf("hold-a-cluster-pg-a message %q", msg)
	}
}

// lostSiteReturnsAsPrimary runs a site loss toward b with site a down, and
// Git down when gitDown is set, then brings a back still primary.
func (f *fixture) lostSiteReturnsAsPrimary(gitDown bool) {
	f.t.Helper()
	f.clients.SetDown("a", true)
	f.git.Unreachable = gitDown
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if gitDown && !errors.Is(err, engine.ErrWaiting) || !gitDown && err != nil {
		f.t.Fatalf("run: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	if st := stepState(sf, "hold-a-cluster-pg-a"); st != failoverv1.StepDeferred {
		f.t.Fatalf("hold-a-cluster-pg-a is %s, want Deferred", st)
	}
	f.clients.SetDown("a", false)
}

// versions returns the resourceVersion of a site's Cluster and Pooler. The
// same versions mean nothing wrote to them.
func (f *fixture) versions(site string) string {
	f.t.Helper()
	var out []string
	for _, kind := range []string{"Cluster", "Pooler"} {
		name := "pg-" + site
		if kind == "Pooler" {
			name += "-pooler-rw"
		}
		u := testenv.Object("postgresql.cnpg.io/v1", kind, "db", name, nil)
		if err := f.clients.Sites[site].Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, kind+"@"+u.GetResourceVersion())
	}
	return strings.Join(out, ",")
}

// stopsUnchanged resumes without --fence and checks that the run stops
// with FencedSiteIsPrimary, before step id, and leaves site a unchanged:
// no write to its Cluster or Pooler, and no hold.
func (f *fixture) stopsUnchanged(id string, want failoverv1.StepState) *failoverv1.SiteFailover {
	f.t.Helper()
	before, holds := f.versions("a"), holdsAt(f.flux, "a")
	sf, err := f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary {
		f.t.Fatalf("err %v, reason %s", err, sf.Status.Reason)
	}
	if !strings.HasPrefix(sf.Status.Message, "step "+id+": site a answers as primary") {
		f.t.Errorf("message %q, want a stop before %s", sf.Status.Message, id)
	}
	if st := stepState(sf, id); st != want {
		f.t.Errorf("%s is %s, want %s", id, st, want)
	}
	if after := f.versions("a"); after != before {
		f.t.Errorf("site a was written to without --fence: %s, then %s", before, after)
	}
	if n := holdsAt(f.flux, "a"); n != holds {
		f.t.Errorf("%d holds at site a without --fence", n-holds)
	}
	if field(f.cluster("a"), "spec", "replica", "primary") != "pg-a" {
		f.t.Error("site a was fenced without --fence")
	}
	return sf
}

// fenceAndFinish resumes with --fence a and Git up, and checks that the run
// succeeds with site a held before its fence and then fenced.
func (f *fixture) fenceAndFinish() {
	f.t.Helper()
	f.git.Unreachable = false
	sf, err := f.resume("b", engine.Options{Fence: []string{"a"}})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		f.t.Fatalf("resume --fence a: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	if field(f.cluster("a"), "spec", "replica", "primary") != "pg-b" {
		f.t.Error("site a was not fenced")
	}
	for _, st := range sf.Status.Steps {
		if st.State != failoverv1.StepSucceeded {
			f.t.Errorf("step %s is %s", st.ID, st.State)
		}
	}
}

// Issue #23. A lost site comes back still primary. Two resumes in a row
// without --fence each stop before its first step and change nothing
// there; the step stays Deferred.
func TestFencedSiteUnchangedOnRepeatedResume(t *testing.T) {
	f := newFixture(t)
	f.lostSiteReturnsAsPrimary(false)
	for i := 0; i < 2; i++ {
		sf := f.stopsUnchanged("hold-a-cluster-pg-a", failoverv1.StepDeferred)
		if msg := stepMessage(sf, "hold-a-cluster-pg-a"); !strings.Contains(msg, "resume with --fence a") {
			t.Errorf("hold-a-cluster-pg-a message %q", msg)
		}
	}
	f.fenceAndFinish()
}

// An approval to fence lasts one process. A resume with --fence a fails at
// a step at site a before its fence; a later resume without --fence stops
// before that Failed step.
func TestFenceApprovalLastsOneProcessFailedStep(t *testing.T) {
	f := newFixture(t)
	f.lostSiteReturnsAsPrimary(false)
	f.flux.Fail["Hold"] = errors.New("hold failed")
	sf, err := f.resume("b", engine.Options{Fence: []string{"a"}})
	if !errors.Is(err, engine.ErrStopped) || stepState(sf, "hold-a-cluster-pg-a") != failoverv1.StepFailed {
		t.Fatalf("err %v, hold-a-cluster-pg-a %s", err, stepState(sf, "hold-a-cluster-pg-a"))
	}
	delete(f.flux.Fail, "Hold")
	f.stopsUnchanged("hold-a-cluster-pg-a", failoverv1.StepFailed)
	f.fenceAndFinish()
}

// An approval to fence lasts one process. A resume with --fence a holds
// site a, then stops at site b before a's fence; a later resume without
// --fence stops before the fence, which was never Deferred.
func TestFenceApprovalLastsOneProcessPendingStep(t *testing.T) {
	f := newFixture(t)
	f.flux.Held["b/Cluster/pg-b"] = "run-0"
	f.clients.SetDown("a", true)
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonHeldByAnotherRun {
		t.Fatalf("err %v, reason %s", err, sf.Status.Reason)
	}
	f.clients.SetDown("a", false)
	sf, err = f.resume("b", engine.Options{Fence: []string{"a"}})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonHeldByAnotherRun {
		t.Fatalf("err %v, reason %s", err, sf.Status.Reason)
	}
	if stepState(sf, "reconcile-hold-a") != failoverv1.StepSucceeded || stepState(sf, "fence-a") != failoverv1.StepPending {
		t.Fatalf("reconcile-hold-a %s, fence-a %s", stepState(sf, "reconcile-hold-a"), stepState(sf, "fence-a"))
	}
	delete(f.flux.Held, "b/Cluster/pg-b")
	sf = f.stopsUnchanged("fence-a", failoverv1.StepPending)
	if stepState(sf, "hold-b-cluster-pg-b") != failoverv1.StepSucceeded {
		t.Errorf("hold-b-cluster-pg-b is %s: the steps before the fence should run", stepState(sf, "hold-b-cluster-pg-b"))
	}
	f.fenceAndFinish()
}

// A process dies while it runs a step at a returning site, before the
// step changed anything. The step is Running; a resume without --fence
// stops before it while the site is still primary.
func TestFencedSiteRunningStep(t *testing.T) {
	f := newFixture(t)
	f.lostSiteReturnsAsPrimary(true)
	sf, err := f.resume("b", engine.Options{Fence: []string{"a"}})
	if !errors.Is(err, engine.ErrWaiting) || stepState(sf, "fence-a") != failoverv1.StepSucceeded {
		t.Fatalf("err %v, fence-a %s", err, stepState(sf, "fence-a"))
	}
	// The fence did not reach site a, and the process died while it ran.
	setPrimary(t, f, "a", "pg-a")
	setStepState(t, f, "fence-a", failoverv1.StepRunning)
	f.stopsUnchanged("fence-a", failoverv1.StepRunning)
	f.fenceAndFinish()
}

// The site stops being primary after the stop, fenced by hand. A resume
// without --fence then runs its steps.
func TestFencedSiteStoppedBeingPrimary(t *testing.T) {
	f := newFixture(t)
	f.lostSiteReturnsAsPrimary(false)
	f.stopsUnchanged("hold-a-cluster-pg-a", failoverv1.StepDeferred)
	setPrimary(t, f, "a", "pg-b")
	sf, err := f.resume("b", engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	for _, st := range sf.Status.Steps {
		if st.State != failoverv1.StepSucceeded {
			t.Errorf("step %s is %s", st.ID, st.State)
		}
	}
	if holdsAt(f.flux, "a") == 0 {
		t.Error("site a was not held")
	}
}

// A site lost at plan time answers as primary when the run starts. The run
// stops before its first step there, as a resume does.
func TestRunStopsWhenLostSiteAnswersAsPrimary(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	plan := f.plan("b", failoverv1.ModeSiteLoss)
	f.clients.SetDown("a", false)
	before := f.versions("a")
	sf, err := f.run("b", plan, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary {
		t.Fatalf("err %v, reason %s", err, sf.Status.Reason)
	}
	if st := stepState(sf, "hold-a-cluster-pg-a"); st != failoverv1.StepPending {
		t.Errorf("hold-a-cluster-pg-a is %s, want Pending", st)
	}
	if after := f.versions("a"); after != before || holdsAt(f.flux, "a") != 0 {
		t.Errorf("site a was changed: %s, then %s; %d holds", before, after, holdsAt(f.flux, "a"))
	}
	f.fenceAndFinish()
}

// A site answers the check before its step, then not the read before it.
// A step that was not Deferred is deferred too.
func TestFenceCheckCannotReadDefersPendingStep(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	plan := f.plan("b", failoverv1.ModeSiteLoss)
	f.clients.SetDown("a", false)
	f.clients.Flap("a", true, false, false, false)
	sf, err := f.run("b", plan, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	for _, id := range []string{"hold-a-cluster-pg-a", "fence-a"} {
		if st := stepState(sf, id); st != failoverv1.StepDeferred {
			t.Errorf("%s is %s, want Deferred", id, st)
		}
	}
	if msg := stepMessage(sf, "hold-a-cluster-pg-a"); !strings.Contains(msg, "did not answer the read before its steps") {
		t.Errorf("hold-a-cluster-pg-a message %q", msg)
	}
	if field(f.cluster("a"), "spec", "replica", "primary") != "pg-a" || holdsAt(f.flux, "a") != 0 {
		t.Error("site a was changed although the read could not read it")
	}
}

// The read before a site's steps fails with an error, not a lost site. The
// run stops before the step, which stays Deferred; the next resume reads
// the site again.
func TestFenceCheckErrorKeepsStepDeferred(t *testing.T) {
	f := newFixture(t)
	f.lostSiteReturnsAsPrimary(false)
	ctx := context.Background()
	p := testenv.Pooler("a", 2)
	if err := f.clients.Sites["a"].Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	sf, err := f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonStepFailed || !strings.Contains(sf.Status.Message, "read Pooler") {
		t.Fatalf("err %v, reason %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if st := stepState(sf, "hold-a-cluster-pg-a"); st != failoverv1.StepDeferred {
		t.Errorf("hold-a-cluster-pg-a is %s, want Deferred", st)
	}
	if holdsAt(f.flux, "a") != 0 {
		t.Error("site a was held")
	}
	if err := f.clients.Sites["a"].Create(ctx, testenv.Pooler("a", 2)); err != nil {
		t.Fatal(err)
	}
	f.stopsUnchanged("hold-a-cluster-pg-a", failoverv1.StepDeferred)
}

func holdsAt(f *testenv.Flux, site string) int {
	n := 0
	for _, c := range calls(f, "Hold") {
		if strings.HasPrefix(c, "Hold "+site+" ") {
			n++
		}
	}
	return n
}

// setPrimary declares a primary in a site's Cluster by hand, and waits for
// the fake operator to follow it.
func setPrimary(t *testing.T, f *fixture, site, primary string) {
	t.Helper()
	ctx := context.Background()
	u := testenv.Object("postgresql.cnpg.io/v1", "Cluster", "db", "pg-"+site, nil)
	patch := fmt.Sprintf(`{"spec":{"replica":{"primary":%q}}}`, primary)
	if err := f.clients.Sites[site].Patch(ctx, u, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatal(err)
	}
	want := primary == "pg-"+site
	for deadline := time.Now().Add(5 * time.Second); ; {
		if field(f.cluster(site), "status", "instancesReportedState", "pg-"+site+"-1", "isPrimary") == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("site %s: isPrimary did not become %v", site, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// setStepState writes a step's state into the ledger at b, as a process
// that died during the step leaves it.
func setStepState(t *testing.T, f *fixture, id string, state failoverv1.StepState) {
	t.Helper()
	sf := f.ledger("b")
	for i := range sf.Status.Steps {
		if sf.Status.Steps[i].ID == id {
			sf.Status.Steps[i].State = state
		}
	}
	if err := f.env("b").Ledger.UpdateStatus(context.Background(), sf); err != nil {
		t.Fatal(err)
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
