package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// diesBeforePromotion runs sf toward b in a process that dies as it starts
// promote-b: every step before it succeeded, and promote-b is Pending.
func (f *fixture) diesBeforePromotion(sf *failoverv1.SiteFailover) {
	f.t.Helper()
	env := f.env("b")
	env.Ledger = &diesBefore{Ledger: env.Ledger, step: "promote-b"}
	if err := env.Start(context.Background(), sf, engine.Options{}); err == nil {
		f.t.Fatal("the run went on after its process died")
	}
	sf = f.ledger("b")
	for _, st := range sf.Status.Steps {
		if st.ID == "promote-b" {
			break
		}
		if st.State != failoverv1.StepSucceeded {
			f.t.Fatalf("%s is %s before the promotion", st.ID, st.State)
		}
	}
	if st := stepState(sf, "promote-b"); st != failoverv1.StepPending {
		f.t.Fatalf("promote-b is %s, want Pending", st)
	}
}

// stopsBeforePromotion resumes with opts and checks that the run stops with
// FencedSiteIsPrimary before promote-b, naming site, with b unchanged.
func (f *fixture) stopsBeforePromotion(site string, opts engine.Options) string {
	f.t.Helper()
	before := resourceVersion(f.t, f.clients.Sites["b"], "Cluster", "pg-b")
	sf, err := f.resume("b", opts)
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary {
		f.t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if !strings.HasPrefix(sf.Status.Message, "step promote-b: site "+site+" ") {
		f.t.Errorf("message %q, want a stop before promote-b naming site %s", sf.Status.Message, site)
	}
	if st := stepState(sf, "promote-b"); st != failoverv1.StepPending {
		f.t.Errorf("promote-b is %s, want Pending", st)
	}
	if after := resourceVersion(f.t, f.clients.Sites["b"], "Cluster", "pg-b"); after != before {
		f.t.Errorf("Cluster pg-b changed: resourceVersion %s, then %s", before, after)
	}
	if field(f.cluster("b"), "spec", "replica", "primary") != "pg-a" {
		f.t.Errorf("site b declares %v", field(f.cluster("b"), "spec", "replica", "primary"))
	}
	if field(f.cluster("b"), "status", "instancesReportedState", "pg-b-1", "isPrimary") == true {
		f.t.Error("site b was promoted while another site is primary")
	}
	return sf.Status.Message
}

// heldBy returns the objects run holds, by key.
func heldBy(fl *testenv.Flux, run string) []string {
	var out []string
	for k, v := range fl.Held {
		if v == run {
			out = append(out, k)
		}
	}
	return out
}

// Issue #45. A planned run from a to b dies after demote-a, before
// promote-b. The operator promotes a again by hand and removes the holds
// (docs/runbooks/no-primary-after-demotion.md, option 2). A resume must
// not promote b while a is primary: not with the holds put back, as the
// StepFailed of checkHeld said, nor with --fence a, which no step of the
// run could act on.
func TestResumeAfterHandRecoveryStopsBeforePromotion(t *testing.T) {
	for _, tt := range []struct {
		name string
		// rehold puts the holds back, as the StepFailed message said.
		rehold bool
		opts   engine.Options
	}{
		{name: "holds put back", rehold: true},
		{name: "holds gone"},
		{name: "holds put back, --fence a", rehold: true, opts: engine.Options{Fence: []string{"a"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.diesBeforePromotion(f.plan("b", failoverv1.ModePlanned))
			setPrimary(t, f, "a", "pg-a")
			held := heldBy(f.flux, "run-1")
			if len(held) != 4 {
				t.Fatalf("run-1 holds %v", held)
			}
			for _, k := range held {
				delete(f.flux.Held, k)
			}
			if tt.rehold {
				for _, k := range held {
					f.flux.Held[k] = "run-1"
				}
			}
			msg := f.stopsBeforePromotion("a", tt.opts)
			for _, want := range []string{"site a answers as primary", "docs/runbooks/abandon-run.md"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q lacks %q", msg, want)
				}
			}
			if strings.Contains(msg, "resume with --fence") {
				t.Errorf("message %q invites --fence, which cannot fence site a", msg)
			}
			// Each resume reads the sites again.
			f.stopsBeforePromotion("a", tt.opts)
		})
	}
}

// Issue #45. A site that declares itself primary stops the run before the
// promotion, before its instance reports isPrimary: CloudNativePG
// promotes such a Cluster at once.
func TestResumeStopsBeforePromotionWhenSiteDeclaresItself(t *testing.T) {
	f := newFixture(t)
	f.diesBeforePromotion(f.plan("b", failoverv1.ModePlanned))
	// The fake CloudNativePG stops, so site a only declares itself.
	f.cancel()
	mergePatchCluster(t, f.clients.Sites["a"], "pg-a", `{"spec":{"replica":{"primary":"pg-a"}}}`)
	if field(f.cluster("a"), "status", "instancesReportedState", "pg-a-1", "isPrimary") == true {
		t.Fatal("site a reports isPrimary")
	}
	msg := f.stopsBeforePromotion("a", engine.Options{})
	if !strings.Contains(msg, "site a declares itself primary") {
		t.Errorf("message %q", msg)
	}
}

// Issue #45, site loss. The run fences a, then dies before promote-b. The
// operator promotes a again by hand. Every step at a is done, so no read
// before a's steps runs. A resume must not promote b, with or without
// --fence a: no step of the run would fence a again.
func TestSiteLossResumeStopsBeforePromotionWhenFromPrimaryAgain(t *testing.T) {
	f := newFixture(t)
	// The operator fenced a out of band before the run (docs/runbooks/site-loss.md).
	setPrimary(t, f, "a", "pg-b")
	f.diesBeforePromotion(f.plan("b", failoverv1.ModeSiteLoss))
	if stepState(f.ledger("b"), "fence-a") != failoverv1.StepSucceeded {
		t.Fatal("fence-a did not run")
	}
	setPrimary(t, f, "a", "pg-a")
	for _, opts := range []engine.Options{{}, {Fence: []string{"a"}}} {
		msg := f.stopsBeforePromotion("a", opts)
		if strings.Contains(msg, "resume with --fence") {
			t.Errorf("message %q invites --fence, which cannot fence site a", msg)
		}
	}
}

// A planned run whose old primary gives no answer when the run resumes
// before the promotion: the read before the promotion does not stop it.
// b promotes with a's token, and the run stops at the first step at a, as
// docs/runbooks/no-primary-after-demotion.md option 1 says. Site a gets
// the 3 checks of the check before its first step, and no more.
func TestResumeBeforePromotionWithFromDown(t *testing.T) {
	f := newFixture(t)
	f.diesBeforePromotion(f.plan("b", failoverv1.ModePlanned))
	f.clients.SetDown("a", true)
	before := f.clients.Checks["a"]
	sf, err := f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonStepFailed || !strings.HasPrefix(sf.Status.Message, "step reconcile-git-a: site a is unreachable") {
		t.Fatalf("err %v, %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if st := stepState(sf, "wait-promoted-b"); st != failoverv1.StepSucceeded {
		t.Errorf("wait-promoted-b is %s", st)
	}
	if n := f.clients.Checks["a"] - before; n != 3 {
		t.Errorf("site a checked %d times in the resume, want 3", n)
	}
}

// A site-loss run with a down: the read before the promotion adds no check
// of the lost site. Each check that gets no answer can add 5 s to the
// time without a primary.
func TestSiteLossReadBeforePromotionAddsNoCheck(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	// One in the plan, one before a's first step.
	if n := f.clients.Checks["a"]; n != 2 {
		t.Errorf("site a checked %d times, want 2", n)
	}
}

// planned3 plans a planned switchover from a to b on three sites.
func (s *sites3) planned3(run string) *failoverv1.SiteFailover {
	s.t.Helper()
	p, err := s.env("b").MakePlan(context.Background(), engine.Request{
		Run: run, Workload: s.w.Name, Spec: &s.w.Spec,
		Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned},
	})
	if err != nil {
		s.t.Fatalf("plan %s: %v", run, err)
	}
	return p.SiteFailover
}

// promoteByHand declares a site's Cluster primary, and waits for the fake
// operator to promote it.
func (s *sites3) promoteByHand(site string) {
	s.t.Helper()
	u := testenv.Object("postgresql.cnpg.io/v1", "Cluster", "db", "pg-"+site, nil)
	patch := fmt.Sprintf(`{"spec":{"replica":{"primary":"pg-%s"}}}`, site)
	if err := s.clients.Sites[site].Patch(context.Background(), u, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		s.t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !s.isPrimary(site); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			s.t.Fatalf("site %s was not promoted", site)
		}
	}
}

// Issue #45, three sites. A planned run from a to b holds c, then dies
// before promote-b; c is promoted by hand. The run declares b primary at
// c only after the promotion. A resume without --fence c stops before the
// promotion; with --fence c, the operator accepts that b and c are both
// primary until the run fences c.
func TestResumeStopsBeforePromotionWhenThirdSiteIsPrimary(t *testing.T) {
	s := newSites3(t)
	ctx := context.Background()
	env := s.env("b")
	env.Ledger = &diesBefore{Ledger: env.Ledger, step: "promote-b"}
	if err := env.Start(ctx, s.planned3("run-1"), engine.Options{}); err == nil {
		t.Fatal("the run went on after its process died")
	}
	if st := stepState(s.ledger("b", "run-1"), "reconcile-hold-c"); st != failoverv1.StepSucceeded {
		t.Fatalf("reconcile-hold-c is %s", st)
	}
	s.promoteByHand("c")

	before := resourceVersion(t, s.clients.Sites["b"], "Cluster", "pg-b")
	sf := s.ledger("b", "run-1")
	err := s.env("b").Resume(ctx, sf, engine.Options{})
	sf = s.ledger("b", "run-1")
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary {
		t.Fatalf("err %v, %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if !strings.HasPrefix(sf.Status.Message, "step promote-b: site c answers as primary") || !strings.Contains(sf.Status.Message, "--fence c") {
		t.Errorf("message %q", sf.Status.Message)
	}
	if after := resourceVersion(t, s.clients.Sites["b"], "Cluster", "pg-b"); after != before || s.isPrimary("b") {
		t.Errorf("site b changed: resourceVersion %s, then %s; primary %v", before, after, s.isPrimary("b"))
	}

	if err := s.env("b").Resume(ctx, s.ledger("b", "run-1"), engine.Options{Fence: []string{"c"}}); err != nil {
		t.Fatalf("resume --fence c: %v", err)
	}
	if sf = s.ledger("b", "run-1"); sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("state %s: %s", sf.Status.State, sf.Status.Message)
	}
	for site, want := range map[string]bool{"a": false, "b": true, "c": false} {
		if got := s.isPrimary(site); got != want {
			t.Errorf("site %s primary %v, want %v", site, got, want)
		}
	}
}
