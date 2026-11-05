package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// diesAt runs sf toward b in a process that dies as it starts step: every
// step before it succeeded. With running, the ledger then shows the step
// Running, as a process killed during the step leaves it; otherwise
// Pending.
func (f *fixture) diesAt(sf *failoverv1.SiteFailover, step string, running bool) {
	f.t.Helper()
	env := f.env("b")
	env.Ledger = &diesBefore{Ledger: env.Ledger, step: step}
	if err := env.Start(context.Background(), sf, engine.Options{}); err == nil {
		f.t.Fatal("the run went on after its process died")
	}
	if running {
		setStepState(f.t, f, step, failoverv1.StepRunning)
	}
}

// Issue #46, from a two-site test of v1.2.1. A planned run from a to b is
// killed after demote-a, before the promotion. The operator promotes a
// again by hand (docs/runbooks/no-primary-after-demotion.md, option 2). A
// resume must stop at once with FencedSiteIsPrimary before the run's next
// step, which keeps its state: v1.2.1 waited the whole demotion timeout
// for a token that a no longer publishes, then stopped with StepFailed.
// --fence a does not help: no step of the run would fence a again.
func TestResumeAfterHandRecoveryStopsAfterDemotion(t *testing.T) {
	for _, tt := range []struct {
		name    string
		step    string
		running bool
		opts    engine.Options
	}{
		{name: "killed during wait-demoted-a", step: "wait-demoted-a", running: true},
		{name: "killed during wait-demoted-a, --fence a", step: "wait-demoted-a", running: true, opts: engine.Options{Fence: []string{"a"}}},
		{name: "died before wait-demoted-a", step: "wait-demoted-a"},
		{name: "killed during capture-token", step: "capture-token", running: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.diesAt(f.plan("b", failoverv1.ModePlanned), tt.step, tt.running)
			want := stepState(f.ledger("b"), tt.step)
			setPrimary(t, f, "a", "pg-a")
			before := resourceVersion(t, f.clients.Sites["b"], "Cluster", "pg-b")

			start := time.Now()
			sf, err := f.resume("b", tt.opts)
			if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary {
				t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
			}
			// The fixture's demotion timeout is 5 s.
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("the resume took %s", d)
			}
			if !strings.HasPrefix(sf.Status.Message, "step "+tt.step+": site a answers as primary") {
				t.Errorf("message %q, want a stop before %s naming site a", sf.Status.Message, tt.step)
			}
			if !strings.Contains(sf.Status.Message, "docs/runbooks/abandon-run.md") || strings.Contains(sf.Status.Message, "resume with --fence") {
				t.Errorf("message %q: want the abandon runbook, and no --fence", sf.Status.Message)
			}
			if st := stepState(sf, tt.step); st != want {
				t.Errorf("%s is %s, want %s", tt.step, st, want)
			}
			if after := resourceVersion(t, f.clients.Sites["b"], "Cluster", "pg-b"); after != before {
				t.Errorf("Cluster pg-b changed: resourceVersion %s, then %s", before, after)
			}
		})
	}
}

// Issue #46. A planned run is killed after it promoted b; a is then
// promoted by hand. Both are primary. A resume stops before its next
// Failover step, and says so: no step of the run is left to fence a.
func TestResumeAfterPromotionStopsWhenOldPrimaryIsPrimaryAgain(t *testing.T) {
	f := newFixture(t)
	f.diesAt(f.plan("b", failoverv1.ModePlanned), "wait-promoted-b", true)
	setPrimary(t, f, "a", "pg-a")
	sf, err := f.resume("b", engine.Options{Fence: []string{"a"}})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	for _, want := range []string{"step wait-promoted-b: site a answers as primary", "which this run promoted: two primaries", "docs/runbooks/abandon-run.md"} {
		if !strings.Contains(sf.Status.Message, want) {
			t.Errorf("message %q lacks %q", sf.Status.Message, want)
		}
	}
	if st := stepState(sf, "wait-promoted-b"); st != failoverv1.StepRunning {
		t.Errorf("wait-promoted-b is %s, want Running", st)
	}
	if st := stepState(sf, "git-commit"); st != failoverv1.StepPending {
		t.Errorf("git-commit is %s, want Pending", st)
	}
}

// Issue #46, site loss. The run fences a, then dies before a's Pooler
// step; a is promoted by hand. The read of the other sites runs before the
// read before a's steps, so the first stop already says that --fence
// cannot help: v1.2.1 first asked for --fence a, then stopped again
// before the promotion.
func TestSiteLossResumeAfterFenceGivesOneWayOn(t *testing.T) {
	f := newFixture(t)
	// The operator fenced a out of band before the run (docs/runbooks/site-loss.md).
	setPrimary(t, f, "a", "pg-b")
	f.diesAt(f.plan("b", failoverv1.ModeSiteLoss), "pooler-a", false)
	if st := stepState(f.ledger("b"), "fence-a"); st != failoverv1.StepSucceeded {
		t.Fatalf("fence-a is %s", st)
	}
	setPrimary(t, f, "a", "pg-a")
	for _, opts := range []engine.Options{{}, {Fence: []string{"a"}}} {
		sf, err := f.resume("b", opts)
		if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary {
			t.Fatalf("--fence %v: err %v, %s %s: %s", opts.Fence, err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
		}
		if !strings.HasPrefix(sf.Status.Message, "step pooler-a: site a answers as primary") || strings.Contains(sf.Status.Message, "resume with --fence") {
			t.Errorf("--fence %v: message %q, want a stop before pooler-a without --fence", opts.Fence, sf.Status.Message)
		}
		if st := stepState(sf, "pooler-a"); st != failoverv1.StepPending {
			t.Errorf("pooler-a is %s, want Pending", st)
		}
	}
}

// Issue #46, three sites. A planned run from a to b declares b primary at
// c, then dies before b's Pooler step; c is promoted by hand. The step that
// fences c is done, so --fence c cannot help: the resume stops, though the
// run still scales c's Pooler later.
func TestResumeStopsWhenThirdSiteIsPrimaryAfterItsDeclaration(t *testing.T) {
	s := newSites3(t)
	ctx := context.Background()
	env := s.env("b")
	env.Ledger = &diesBefore{Ledger: env.Ledger, step: "pooler-b"}
	if err := env.Start(ctx, s.planned3("run-1"), engine.Options{}); err == nil {
		t.Fatal("the run went on after its process died")
	}
	if st := stepState(s.ledger("b", "run-1"), "declare-c"); st != failoverv1.StepSucceeded {
		t.Fatalf("declare-c is %s", st)
	}
	s.promoteByHand("c")
	err := s.env("b").Resume(ctx, s.ledger("b", "run-1"), engine.Options{Fence: []string{"c"}})
	sf := s.ledger("b", "run-1")
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if !strings.HasPrefix(sf.Status.Message, "step pooler-b: site c answers as primary") || !strings.Contains(sf.Status.Message, "--fence cannot help") {
		t.Errorf("message %q", sf.Status.Message)
	}
}
