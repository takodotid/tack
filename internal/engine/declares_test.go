package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// declaresOnly makes site a declare itself primary with no instance that
// reports isPrimary, as a Cluster does before CloudNativePG promotes it:
// only site b's fake operator runs from then on.
func (f *fixture) declaresOnly() {
	f.t.Helper()
	f.cancel()
	// Let the stopped operator's last pass end before the patch.
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	f.t.Cleanup(cancel)
	go (&testenv.Operator{Clients: f.clients, Sites: []string{"b"}}).Run(ctx)
	mergePatchCluster(f.t, f.clients.Sites["a"], "pg-a",
		`{"spec":{"replica":{"primary":"pg-a"}},"status":{"instancesReportedState":{"pg-a-1":{"isPrimary":false}}}}`)
	if field(f.cluster("a"), "status", "instancesReportedState", "pg-a-1", "isPrimary") == true {
		f.t.Fatal("site a reports isPrimary")
	}
}

// #35 row 33. A lost site comes back declaring itself primary, before an
// instance reports isPrimary: CloudNativePG promotes such a Cluster at
// once. The read before its steps stops the run, as for a site that
// answers as primary, and changes nothing there until --fence.
func TestReadBeforeStepsStopsWhenSiteDeclaresItself(t *testing.T) {
	f := newFixture(t)
	f.lostSiteReturnsAsPrimary(false)
	f.declaresOnly()
	for i := 0; i < 2; i++ {
		before, holds := f.versions("a"), holdsAt(f.flux, "a")
		sf, err := f.resume("b", engine.Options{})
		if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary {
			t.Fatalf("resume %d: err %v, %s %s: %s", i+1, err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
		}
		if !strings.HasPrefix(sf.Status.Message, "step hold-a-cluster-pg-a: site a declares itself primary") ||
			!strings.Contains(sf.Status.Message, "resume with --fence a") {
			t.Errorf("message %q", sf.Status.Message)
		}
		if st := stepState(sf, "hold-a-cluster-pg-a"); st != failoverv1.StepDeferred {
			t.Errorf("hold-a-cluster-pg-a is %s, want Deferred", st)
		}
		if after := f.versions("a"); after != before || holdsAt(f.flux, "a") != holds {
			t.Errorf("site a was changed without --fence: %s, then %s; %d holds", before, after, holdsAt(f.flux, "a")-holds)
		}
	}
	// CloudNativePG at site a runs again, and the operator fences a.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go (&testenv.Operator{Clients: f.clients, Sites: []string{"a"}}).Run(ctx)
	sf, err := f.resume("b", engine.Options{Fence: []string{"a"}})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume --fence a: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	if field(f.cluster("a"), "spec", "replica", "primary") != "pg-b" {
		t.Error("site a was not fenced")
	}
}

// #35 row 33. A site-loss plan whose from answers and declares itself
// primary, with no instance that reports it, is not refused: the driver
// refuses only a from that answers as primary ([RFC#0004] Requirement 5).
// The plan says that the run stops before from's first step, and the run
// does.
func TestSiteLossPlanNotesSiteThatDeclaresItself(t *testing.T) {
	f := newFixture(t)
	f.declaresOnly()
	p, err := f.env("b").MakePlan(context.Background(), engine.Request{
		Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
		Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	found := false
	for _, n := range p.Notes {
		found = found || strings.HasPrefix(n, "site a declares itself primary") && strings.Contains(n, "--fence a")
	}
	if !found {
		t.Errorf("no note on site a: %q", p.Notes)
	}
	before := f.versions("a")
	sf, err := f.run("b", p.SiteFailover, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFencedSiteIsPrimary ||
		!strings.HasPrefix(sf.Status.Message, "step hold-a-cluster-pg-a: site a declares itself primary") {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if after := f.versions("a"); after != before || holdsAt(f.flux, "a") != 0 {
		t.Errorf("site a was changed: %s, then %s; %d holds", before, after, holdsAt(f.flux, "a"))
	}
}
