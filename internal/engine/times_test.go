package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

func stepStatus(sf *failoverv1.SiteFailover, id string) failoverv1.StepStatus {
	for _, s := range sf.Status.Steps {
		if s.ID == id {
			return s
		}
	}
	return failoverv1.StepStatus{}
}

func phaseStatus(sf *failoverv1.SiteFailover, p failoverv1.Phase) failoverv1.PhaseStatus {
	for _, s := range sf.Status.Phases {
		if s.Name == p {
			return s
		}
	}
	return failoverv1.PhaseStatus{}
}

func at(t *metav1.Time) string {
	if t == nil {
		return "unset"
	}
	return t.UTC().Format(time.RFC3339)
}

// #35 row 17 (RFC0003 Requirements 4 and 12). A step that ends Failed gets
// finishedAt, after its startedAt and with its attempt.
func TestFailedStepFinishes(t *testing.T) {
	f := newFixture(t)
	f.flux.Fail["VerifyNoop"] = errors.New("the API server timed out")
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run: %v", err)
	}
	st := stepStatus(sf, "verify-noop-a")
	if st.State != failoverv1.StepFailed || st.StartedAt == nil || st.FinishedAt == nil || st.Attempts != 1 ||
		st.FinishedAt.Before(st.StartedAt) {
		t.Errorf("verify-noop-a: %s, started %s, finished %s, %d attempts; want Failed, finished after its start, 1 attempt",
			st.State, at(st.StartedAt), at(st.FinishedAt), st.Attempts)
	}
	if p := phaseStatus(sf, failoverv1.PhaseRelease); p.FinishedAt != nil {
		t.Errorf("phase Release finished at %s with a failed step", at(p.FinishedAt))
	}
}

// A site-loss run toward a, with site b lost: b's steps come last in each
// phase. Each deferred step gets finishedAt, and so does each phase once
// its other steps succeeded. When b comes back, its steps run: the phases
// they are in finish again, at that process's time.
func TestDeferredStepsFinishTheirPhases(t *testing.T) {
	f := newFixture(t)
	setPrimary(t, f, "b", "pg-b")
	setPrimary(t, f, "a", "pg-b")
	f.clients.SetDown("b", true)
	first := time.Date(2026, 10, 27, 10, 15, 0, 0, time.UTC)
	env := f.env("a")
	env.Now = func() time.Time { return first }
	ctx := context.Background()
	plan := f.plan("a", failoverv1.ModeSiteLoss)
	if err := env.Start(ctx, plan, engine.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	sf := f.ledger("a")
	if sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %s: %s", sf.Status.State, sf.Status.Message)
	}
	for _, s := range sf.Spec.Steps {
		st := stepStatus(sf, s.ID)
		if s.Site == "b" && (st.State != failoverv1.StepDeferred || st.FinishedAt == nil || !st.FinishedAt.Time.Equal(first) || st.Attempts != 0) {
			t.Errorf("%s: %s, finished %s, %d attempts; want Deferred at %s, never attempted", s.ID, st.State, at(st.FinishedAt), st.Attempts, first.Format(time.RFC3339))
		}
	}
	for _, p := range failoverv1.Phases {
		if ps := phaseStatus(sf, p); ps.FinishedAt == nil || !ps.FinishedAt.Time.Equal(first) {
			t.Errorf("phase %s finished %s, want %s", p, at(ps.FinishedAt), first.Format(time.RFC3339))
		}
	}

	// Site b comes back, primary; the operator fences it an hour later.
	f.clients.SetDown("b", false)
	later := first.Add(time.Hour)
	env.Now = func() time.Time { return later }
	if err := env.Resume(ctx, f.ledger("a"), engine.Options{Fence: []string{"b"}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	sf = f.ledger("a")
	for _, p := range failoverv1.Phases {
		if ps := phaseStatus(sf, p); ps.FinishedAt == nil || !ps.FinishedAt.Time.Equal(later) || !ps.StartedAt.Time.Equal(first) {
			t.Errorf("phase %s: started %s, finished %s; want started %s, finished %s once b's steps ran",
				p, at(ps.StartedAt), at(ps.FinishedAt), first.Format(time.RFC3339), later.Format(time.RFC3339))
		}
	}
	if st := stepStatus(sf, "hold-b-cluster-pg-b"); st.State != failoverv1.StepSucceeded || !st.FinishedAt.Time.Equal(later) || st.Attempts != 1 {
		t.Errorf("hold-b-cluster-pg-b: %s, finished %s, %d attempts", st.State, at(st.FinishedAt), st.Attempts)
	}
}

// PR #42's finding (RFC0003 Requirement 12). v1.2.0 ended a phase only
// after a step of it succeeded, so a site-loss run whose lost site's steps
// came last in a phase left that phase unfinished. The ledger below is
// such a run, as v1.2.0 wrote it: b lost, Hold, PromoteToGitOps and
// Release unfinished. A resume while b stays down ends them.
func TestResumeEndsPhasesLeftUnfinished(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("b", true)
	sf := loadLedger(t, "testdata/ledgers/v1.2.0/site-loss-lost-site-last.yaml")
	sf.Status.State = failoverv1.RunSucceeded
	unfinished := 0
	for _, p := range sf.Status.Phases {
		if p.FinishedAt == nil {
			unfinished++
		}
	}
	if unfinished != 3 {
		t.Fatalf("the v1.2.0 ledger has %d unfinished phases, want 3", unfinished)
	}
	ledger := f.env("a").Ledger
	status := sf.Status
	if err := ledger.Create(context.Background(), sf); err != nil {
		t.Fatal(err)
	}
	sf.Status = status
	if err := ledger.UpdateStatus(context.Background(), sf); err != nil {
		t.Fatal(err)
	}
	got, err := f.resume("a", engine.Options{})
	if err != nil || got.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("err %v, %s: %s", err, got.Status.State, got.Status.Message)
	}
	for _, p := range got.Status.Phases {
		if p.FinishedAt == nil {
			t.Errorf("phase %s is unfinished", p.Name)
		}
	}
}
