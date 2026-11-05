package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
)

// RFC0003 Requirement 14. An earlier run left site b to rebuild, and b's
// member was not created again since. A site-loss plan toward b is not
// refused, but notes it.
func TestSiteLossPlanNotesPendingRebuild(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	started := metav1.NewTime(time.Now().Add(time.Minute))
	earlier := failoverv1.SiteFailover{
		ObjectMeta: metav1.ObjectMeta{Name: "run-0", Namespace: "tack-system"},
		Status: failoverv1.SiteFailoverStatus{State: failoverv1.RunSucceeded, StartedAt: &started,
			FollowUps: []failoverv1.FollowUp{{Site: "b", Action: engine.FollowUpRebuild, Reason: "promoted without a token"}}},
	}
	env := f.env("b")
	env.Runs = func(context.Context, kube.Clients) ([]failoverv1.SiteFailover, error) {
		return []failoverv1.SiteFailover{earlier}, nil
	}
	p, err := env.MakePlan(context.Background(), engine.Request{
		Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
		Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := "run run-0 left site b to rebuild (promoted without a token), and its member was not created again since"
	found := false
	for _, n := range p.Notes {
		found = found || strings.Contains(n, want)
	}
	if !found {
		t.Errorf("notes %q lack %q", p.Notes, want)
	}
}
