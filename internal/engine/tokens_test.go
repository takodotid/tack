package engine_test

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// diesBefore is a ledger whose process dies as it starts step: the write
// that records the step Running fails.
type diesBefore struct {
	engine.Ledger
	step string
}

func (l *diesBefore) UpdateStatus(ctx context.Context, sf *failoverv1.SiteFailover) error {
	for _, st := range sf.Status.Steps {
		if st.ID == l.step && st.State == failoverv1.StepRunning {
			return errors.New("the process died")
		}
	}
	return l.Ledger.UpdateStatus(ctx, sf)
}

// Issue #34. RFC0002 Requirement 5: a captured value that is absent or
// empty when a step needs it stops the run with TokenMissing, and the
// promotion does not run. So does a capture that reads an empty field.
func TestTokenMissing(t *testing.T) {
	for _, tt := range []struct {
		name string
		// dieBefore is the step the first process dies at.
		dieBefore string
		// edit changes the ledger or site a before the resume.
		edit func(f *fixture, sf *failoverv1.SiteFailover)
		// step is the step that stops the run.
		step string
	}{
		{name: "absent", dieBefore: "promote-b", step: "promote-b",
			edit: func(_ *fixture, sf *failoverv1.SiteFailover) { delete(sf.Status.Captured, "demotionToken") }},
		{name: "empty", dieBefore: "promote-b", step: "promote-b",
			edit: func(_ *fixture, sf *failoverv1.SiteFailover) { sf.Status.Captured["demotionToken"] = "" }},
		{name: "empty field", dieBefore: "capture-token", step: "capture-token",
			edit: func(f *fixture, _ *failoverv1.SiteFailover) {
				mergePatchCluster(f.t, f.clients.Sites["a"], "pg-a", `{"status":{"demotionToken":null}}`)
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			env := f.env("b")
			env.Ledger = &diesBefore{Ledger: env.Ledger, step: tt.dieBefore}
			if err := env.Start(ctx, f.plan("b", failoverv1.ModePlanned), engine.Options{}); err == nil {
				t.Fatal("the run went on after its process died")
			}
			sf := f.ledger("b")
			tt.edit(f, sf)
			if err := f.env("b").Ledger.UpdateStatus(ctx, sf); err != nil {
				t.Fatal(err)
			}
			before := resourceVersion(t, f.clients.Sites["b"], "Cluster", "pg-b")
			sf, err := f.resume("b", engine.Options{})
			if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonTokenMissing {
				t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
			}
			if st := stepState(sf, tt.step); st != failoverv1.StepFailed {
				t.Errorf("%s is %s, want Failed", tt.step, st)
			}
			if after := resourceVersion(t, f.clients.Sites["b"], "Cluster", "pg-b"); after != before {
				t.Errorf("Cluster pg-b changed: resourceVersion %s, then %s", before, after)
			}
			if field(f.cluster("b"), "status", "instancesReportedState", "pg-b-1", "isPrimary") == true {
				t.Error("site b was promoted without the demotion token")
			}
		})
	}
}

func mergePatchCluster(t *testing.T, c client.Client, name, patch string) {
	t.Helper()
	u := testenv.Object("postgresql.cnpg.io/v1", "Cluster", "db", name, nil)
	if _, err := apiutil.GVKForObject(u, c.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := c.Patch(context.Background(), u, client.RawPatch("application/merge-patch+json", []byte(patch))); err != nil {
		t.Fatal(err)
	}
}
