package engine_test

import (
	"context"
	"errors"
	"io"
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
	}
}

func (f *fixture) start(to string, mode failoverv1.Mode, opts engine.Options) (*failoverv1.SiteFailover, error) {
	f.t.Helper()
	env := f.env(to)
	from := "a"
	if to == "a" {
		from = "b"
	}
	p, err := env.MakePlan(context.Background(), engine.Request{
		Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
		Request: driver.Request{From: from, To: to, Mode: mode},
	})
	if err != nil {
		f.t.Fatalf("plan: %v", err)
	}
	err = env.Start(context.Background(), p.SiteFailover, opts)
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

func TestSiteLossSiteStaysDown(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %v %s", err, sf.Status.Message)
	}
	if !strings.Contains(sf.Status.Message, "wait for their site") {
		t.Errorf("message %q", sf.Status.Message)
	}
	if v, _ := change(f.git.Commits[0], "a", cnpg.FieldPrimary); v != `"pg-b"` {
		t.Errorf("Git does not demote site a: %s", v)
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

func countCalls(f *testenv.Flux, name string) int {
	n := 0
	for _, c := range f.Calls {
		if strings.HasPrefix(c, name+" ") {
			n++
		}
	}
	return n
}
