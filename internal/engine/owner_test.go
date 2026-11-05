package engine_test

import (
	"context"
	"strings"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
)

// RFC0005 Requirement 4: a plan refuses an object that a HelmRelease
// applies, and one that no Kustomization applies. Nothing can hold them.
func TestPlanRefusesObjectsFluxCannotHold(t *testing.T) {
	ks := &failoverv1.ObjectKey{Namespace: "flux-system", Name: "b"}
	for _, tt := range []struct {
		name  string
		owner engine.Owner
		want  string
	}{
		{name: "helm-controller", owner: engine.Owner{Helm: true}, want: "site b: Pooler db/pg-b-pooler-rw is applied by helm-controller"},
		{name: "helm-controller and a Kustomization", owner: engine.Owner{Helm: true, Kustomization: ks}, want: "is applied by helm-controller"},
		{name: "no Kustomization", owner: engine.Owner{}, want: "site b: Pooler db/pg-b-pooler-rw is not applied by a Flux Kustomization"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.flux.Owners = map[string]engine.Owner{"b/Pooler/pg-b-pooler-rw": tt.owner}
			_, err := f.env("b").MakePlan(context.Background(), engine.Request{
				Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
				Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("plan: %v; want an error with %q", err, tt.want)
			}
		})
	}
}
