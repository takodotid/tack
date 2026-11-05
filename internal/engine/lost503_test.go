package engine_test

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/version"
)

// #38 item 8. A lost site behind a load balancer answers 503. An answer
// is never counted as a lost site (#25): the site-loss plan refuses, and
// its message names the runbook's way on. v1.2.1 gave the error alone.
// A planned plan refuses with the error alone: it needs every site.
func TestSiteLossPlanRefusesSiteThatAnswers503(t *testing.T) {
	for _, mode := range []failoverv1.Mode{failoverv1.ModeSiteLoss, failoverv1.ModePlanned} {
		f := newFixture(t)
		f.clients.Fail("a", apierrors.NewServiceUnavailable("no healthy upstream"))
		_, err := f.env("b").MakePlan(context.Background(), engine.Request{
			Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
			Request: driver.Request{From: "a", To: "b", Mode: mode},
		})
		if err == nil || !strings.Contains(err.Error(), "no healthy upstream") {
			t.Fatalf("%s: plan: %v, want a refusal with the site's answer", mode, err)
		}
		runbook := strings.Contains(err.Error(), version.DocLink("docs/runbooks/site-loss.md#when-tack-refuses-the-lost-site"))
		if runbook != (mode == failoverv1.ModeSiteLoss) {
			t.Errorf("%s: plan: %v; names the runbook: %v", mode, err, runbook)
		}
	}
}
