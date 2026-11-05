package engine_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/config"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/driver/cnpg"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/testenv"
)

const workload3YAML = `apiVersion: failover.tako.id/v1
kind: Workload
metadata: {name: pg-main}
spec:
  driver: cnpg
  sites:
  - {name: a, kubeContext: site-a, cnpg: {cluster: {namespace: db, name: pg-a}, pooler: {namespace: db, name: pg-a-pooler-rw, instances: 2}}}
  - {name: b, kubeContext: site-b, cnpg: {cluster: {namespace: db, name: pg-b}, pooler: {namespace: db, name: pg-b-pooler-rw, instances: 2}}}
  - {name: c, kubeContext: site-c, cnpg: {cluster: {namespace: db, name: pg-c}, pooler: {namespace: db, name: pg-c-pooler-rw, instances: 2}}}
  git: {url: https://git.example.com/gitops.git, branch: main, find: {}}
  timeouts: {demotion: 5s, promotion: 5s, flux: 5s}
`

// sites3 is three sites with a primary at a, its fakes, and an Env per
// ledger site whose Runs lists the ledgers at every site that answers.
type sites3 struct {
	t       *testing.T
	w       *config.Workload
	clients *testenv.Clients
	flux    *testenv.Flux
	git     *testenv.Git
}

func newSites3(t *testing.T) *sites3 {
	t.Helper()
	w, err := config.Parse([]byte(workload3YAML))
	if err != nil {
		t.Fatal(err)
	}
	s := &sites3{
		t: t, w: w,
		clients: testenv.NewClients(map[string][]client.Object{
			"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0), "c": testenv.Site("c", "a", 0),
		}),
		flux: testenv.NewFlux("main@sha1:1111111111111111111111111111111111111111"),
		git:  &testenv.Git{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go (&testenv.Operator{Clients: s.clients, Sites: []string{"a", "b", "c"}}).Run(ctx)
	return s
}

func (s *sites3) env(ledgerSite string) *engine.Env {
	return &engine.Env{
		Clients: s.clients, Driver: cnpg.Driver{}, Flux: s.flux, Git: s.git,
		Ledger: &ledger.Store{Client: s.clients.Sites[ledgerSite], Namespace: "tack-system"},
		Runs: func(ctx context.Context, c kube.Clients) ([]failoverv1.SiteFailover, error) {
			var out []failoverv1.SiteFailover
			for _, site := range []string{"a", "b", "c"} {
				if c.Reachable(ctx, site) != nil {
					continue
				}
				l, err := (&ledger.Store{Client: s.clients.Sites[site], Namespace: "tack-system"}).List(ctx)
				if err != nil {
					return nil, err
				}
				out = append(out, l...)
			}
			return out, nil
		},
		Out: io.Discard, Poll: time.Millisecond,
		ReachWaits: []time.Duration{time.Millisecond, time.Millisecond},
	}
}

func (s *sites3) plan(run, from, to string) *failoverv1.SiteFailover {
	s.t.Helper()
	p, err := s.env(to).MakePlan(context.Background(), engine.Request{
		Run: run, Workload: s.w.Name, Spec: &s.w.Spec,
		Request: driver.Request{From: from, To: to, Mode: failoverv1.ModeSiteLoss},
	})
	if err != nil {
		s.t.Fatalf("plan %s: %v", run, err)
	}
	return p.SiteFailover
}

func (s *sites3) ledger(site, run string) *failoverv1.SiteFailover {
	s.t.Helper()
	sf, err := s.env(site).Ledger.Get(context.Background(), run)
	if err != nil {
		s.t.Fatalf("read ledger %s at %s: %v", run, site, err)
	}
	return sf
}

func (s *sites3) isPrimary(site string) bool {
	u := testenv.Object("postgresql.cnpg.io/v1", "Cluster", "db", "pg-"+site, nil)
	if err := s.clients.Sites[site].Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
		s.t.Fatal(err)
	}
	return field(u.Object, "status", "instancesReportedState", "pg-"+site+"-1", "isPrimary") == true
}

// loseAfter is the fake Flux, except that site lost goes down once a
// Reconcile at site after returns: the run loses it between two steps.
type loseAfter struct {
	*testenv.Flux
	clients     *testenv.Clients
	after, lost string
}

func (l *loseAfter) Reconcile(ctx context.Context, site string, ks []failoverv1.ObjectKey, revision string, fetchGit bool, timeout time.Duration) (string, []string, error) {
	rev, notes, err := l.Flux.Reconcile(ctx, site, ks, revision, fetchGit, timeout)
	if site == l.after {
		l.clients.SetDown(l.lost, true)
	}
	return rev, notes, err
}

// Issue #27. run-1, a site loss from a to b, holds b and c, then stops at
// promote-b: site b is lost too. run-2 takes run-1 over and promotes c.
// When b comes back, a resume of run-1 must not promote b: c is the
// primary.
func TestResumeRefusesRunTakenOver(t *testing.T) {
	s := newSites3(t)
	ctx := context.Background()
	s.clients.SetDown("a", true)
	env := s.env("b")
	env.Flux = &loseAfter{Flux: s.flux, clients: s.clients, after: "c", lost: "b"}
	err := env.Start(ctx, s.plan("run-1", "a", "b"), engine.Options{})
	// b is lost before its promotion: the read before the first Failover
	// step at b, or the promotion itself, stops run-1.
	left := stepState(s.ledger("b", "run-1"), "promote-b")
	if !errors.Is(err, engine.ErrStopped) || left == failoverv1.StepSucceeded {
		t.Fatalf("run-1: %v, promote-b %s", err, left)
	}

	if err := s.env("c").Start(ctx, s.plan("run-2", "a", "c"), engine.Options{TakeOver: []string{"run-1"}}); err != nil {
		t.Fatalf("run-2: %v", err)
	}
	if !s.isPrimary("c") {
		t.Fatal("run-2 did not promote c")
	}

	s.clients.SetDown("b", false)
	err = s.env("b").Resume(ctx, s.ledger("b", "run-1"), engine.Options{})
	if err == nil {
		t.Error("the resume of run-1 was not refused")
	}
	if s.isPrimary("b") {
		t.Fatal("the resume of run-1 promoted b while c is primary")
	}
	if got := stepState(s.ledger("b", "run-1"), "promote-b"); got != left {
		t.Errorf("promote-b is %s, want %s as run-1 left it", got, left)
	}
}

// A run that took another over takes over its holds in every process: a
// resume of run-2 holds b, which run-1 still holds, and finishes there.
func TestResumeTakesOverHoldsFromSpec(t *testing.T) {
	s := newSites3(t)
	ctx := context.Background()
	s.clients.SetDown("a", true)
	env := s.env("b")
	env.Flux = &loseAfter{Flux: s.flux, clients: s.clients, after: "c", lost: "b"}
	if err := env.Start(ctx, s.plan("run-1", "a", "b"), engine.Options{}); !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run-1: %v", err)
	}
	if err := s.env("c").Start(ctx, s.plan("run-2", "a", "c"), engine.Options{TakeOver: []string{"run-1"}}); err != nil {
		t.Fatalf("run-2: %v", err)
	}
	if got := s.ledger("c", "run-2").Spec.TakeOver; got != "run-1" {
		t.Errorf("spec.takeOver %q, want run-1", got)
	}
	s.clients.SetDown("b", false)
	if err := s.env("c").Resume(ctx, s.ledger("c", "run-2"), engine.Options{}); err != nil {
		t.Fatalf("resume run-2: %v", err)
	}
	sf := s.ledger("c", "run-2")
	if st := stepState(sf, "hold-b-cluster-pg-b"); st != failoverv1.StepSucceeded {
		t.Errorf("hold-b-cluster-pg-b is %s: %s", st, stepMessage(sf, "hold-b-cluster-pg-b"))
	}
	if by := s.flux.Held["b/Cluster/pg-b"]; by != "" {
		t.Errorf("Cluster pg-b still held by %s", by)
	}
}

// A run is refused when another run of the workload started after it, or
// its own ledger says another run took it over.
func TestResumeRefusesOlderRun(t *testing.T) {
	f := newFixture(t)
	f.flux.Fail["VerifyGit"] = errors.New("stop here")
	if _, err := f.start("b", failoverv1.ModePlanned, engine.Options{}); !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run-1: %v", err)
	}
	delete(f.flux.Fail, "VerifyGit")
	ctx := context.Background()
	sf := f.ledger("b")
	sf.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	if err := f.clients.Sites["b"].Update(ctx, sf); err != nil {
		t.Fatal(err)
	}
	newer := &failoverv1.SiteFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "run-9", Namespace: "tack-system", CreationTimestamp: metav1.Now()},
		Spec:       failoverv1.SiteFailoverSpec{Mode: failoverv1.ModePlanned, From: "b", To: "a"},
	}
	if err := f.clients.Sites["a"].Create(ctx, newer); err != nil {
		t.Fatal(err)
	}
	env := f.env("b")
	env.Runs = func(ctx context.Context, c kube.Clients) ([]failoverv1.SiteFailover, error) {
		var out []failoverv1.SiteFailover
		for _, site := range []string{"a", "b"} {
			l, err := (&ledger.Store{Client: f.clients.Sites[site], Namespace: "tack-system"}).List(ctx)
			if err != nil {
				return nil, err
			}
			out = append(out, l...)
		}
		return out, nil
	}
	err := env.Resume(ctx, f.ledger("b"), engine.Options{})
	if !errors.Is(err, engine.ErrCannotResume) || !strings.Contains(err.Error(), "run run-9 started after run run-1") {
		t.Fatalf("err %v, want a refusal naming run-9", err)
	}
	if stepState(f.ledger("b"), "verify-git-a") != failoverv1.StepFailed {
		t.Error("the refused resume ran a step")
	}

	env.Runs = nil
	sf = f.ledger("b")
	sf.Status.SupersededBy = "run-9"
	if err := env.Ledger.UpdateStatus(ctx, sf); err != nil {
		t.Fatal(err)
	}
	if err := env.Resume(ctx, f.ledger("b"), engine.Options{}); !errors.Is(err, engine.ErrCannotResume) || !strings.Contains(err.Error(), "taken over by run run-9") {
		t.Fatalf("err %v, want a refusal from status.supersededBy", err)
	}
}

// Before a patch, the run checks that it still holds the object. A run
// whose hold another run took over stops and changes nothing; so does a
// run whose hold is gone.
func TestPatchNeedsTheRunsHold(t *testing.T) {
	f := newFixture(t)
	f.flux.Fail["Reconcile"] = errors.New("stop after the holds")
	if _, err := f.start("b", failoverv1.ModePlanned, engine.Options{}); !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run-1: %v", err)
	}
	delete(f.flux.Fail, "Reconcile")
	before := f.versions("a")
	f.flux.Held["a/Pooler/pg-a-pooler-rw"] = "run-2"
	sf, err := f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonHeldByAnotherRun || !strings.Contains(sf.Status.Message, "held by run run-2") {
		t.Fatalf("err %v, reason %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if after := f.versions("a"); after != before {
		t.Errorf("site a changed: %s, then %s", before, after)
	}

	delete(f.flux.Held, "a/Pooler/pg-a-pooler-rw")
	sf, err = f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonStepFailed || !strings.Contains(sf.Status.Message, "no longer held by this run") {
		t.Fatalf("err %v, reason %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	// Issue #45: holding the object again resumes a plan that a recovery by
	// hand may have made stale. The message says so before it says how.
	stale, again := strings.Index(sf.Status.Message, "the run's plan may be stale"), strings.Index(sf.Status.Message, "annotate it again")
	if stale < 0 || again < stale || !strings.Contains(sf.Status.Message, "docs/runbooks/abandon-run.md") {
		t.Errorf("message %q: it must say that the plan may be stale, and name docs/runbooks/abandon-run.md, before it says to hold the object again", sf.Status.Message)
	}
	if after := f.versions("a"); after != before {
		t.Errorf("site a changed: %s, then %s", before, after)
	}
}
