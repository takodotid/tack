package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
)

// RFC0002 Requirement 8: Compose refuses a plan whose step IDs repeat.
func TestComposeRefusesRepeatedStepID(t *testing.T) {
	f := newFixture(t)
	value := `"pg-b"`
	patch := func(id string) failoverv1.Step {
		return failoverv1.Step{ID: id, Type: failoverv1.StepPatch, Site: "a",
			Patch: &failoverv1.PatchStep{Object: failoverv1.ObjectRef{Kind: "Cluster", Namespace: "db", Name: "pg-a"}, Merge: `{}`}}
	}
	in := engine.Inputs{
		Request:    engine.Request{Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec, Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}},
		Topology:   &driver.Topology{Sites: []driver.SiteState{{Name: "a"}, {Name: "b"}}},
		GitChanges: []failoverv1.GitChange{{Site: "a", Object: failoverv1.ObjectRef{Kind: "Cluster", Namespace: "db", Name: "pg-a"}, Field: "spec.replica.primary", Value: &value}},
	}
	in.Failover = []failoverv1.Step{patch("demote-a"), patch("wait-a")}
	if _, err := engine.Compose(in); err != nil {
		t.Fatalf("distinct IDs: %v", err)
	}
	in.Failover = []failoverv1.Step{patch("demote-a"), patch("demote-a")}
	if _, err := engine.Compose(in); err == nil || !strings.Contains(err.Error(), `step ID "demote-a" is repeated`) {
		t.Fatalf("repeated ID: %v", err)
	}
}

// RFC0005 Requirement 5, RFC0002 Requirement 3: every Patch step writes
// with field manager tack.
func TestPatchUsesFieldManagerTack(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var managers []string
	for _, site := range []string{"a", "b"} {
		cl := f.clients.Sites[site].(client.WithWatch)
		f.clients.SetSite(site, interceptor.NewClient(cl, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				o := &client.PatchOptions{}
				o.ApplyOptions(opts)
				mu.Lock()
				managers = append(managers, obj.GetObjectKind().GroupVersionKind().Kind+" "+obj.GetName()+": "+o.FieldManager)
				mu.Unlock()
				return c.Patch(ctx, obj, p, opts...)
			},
		}))
	}
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %v %s", err, sf.Status.Message)
	}
	patches := 0
	for _, s := range sf.Spec.Steps {
		if s.Type == failoverv1.StepPatch {
			patches++
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(managers) != patches {
		t.Errorf("%d patches for %d Patch steps: %v", len(managers), patches, managers)
	}
	for _, m := range managers {
		if !strings.HasSuffix(m, ": tack") {
			t.Errorf("patch of %s, want field manager tack", m)
		}
	}
}
