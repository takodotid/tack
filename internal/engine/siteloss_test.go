package engine_test

import (
	"context"
	"strings"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/version"
)

// Issue #26. Tack cannot tell a lost site from one cut off from the
// operator's host. Every site-loss plan says so, and names the runbook.
func TestSiteLossPlanNotesRunbook(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	p, err := f.env("b").MakePlan(context.Background(), engine.Request{
		Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
		Request: driver.Request{To: "b", Mode: failoverv1.ModeSiteLoss},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	found := false
	for _, n := range p.Notes {
		found = found || strings.Contains(n, "cut off from this host") && strings.Contains(n, "site a") && strings.Contains(n, version.DocLink("docs/runbooks/site-loss.md"))
	}
	if !found {
		t.Errorf("notes %q lack the warning", p.Notes)
	}
}
