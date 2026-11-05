package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// poolerInstances reads spec.instances of a site's Pooler.
func poolerInstances(t *testing.T, f *fixture, site string) any {
	t.Helper()
	u := testenv.Object("postgresql.cnpg.io/v1", "Pooler", "db", "pg-"+site+"-pooler-rw", nil)
	if err := f.clients.Sites[site].Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
		t.Fatal(err)
	}
	return field(u.Object, "spec", "instances")
}

// #46 (PR #48's comment). The read after the confirmation of tack run
// (RFC0009 Requirement 13) did not run in tack resume. A planned run that
// stopped in Hold, before any Failover step, then resumed after the
// operator switched the primary by hand, scaled a's Pooler to 0 and
// demoted a, a replica, then waited for a token a never published. Before
// its first Failover step, the run now reads the sites again, and stops
// when the driver no longer allows the run.
func TestResumeStopsBeforeFailoverWhenTheSitesChanged(t *testing.T) {
	f := stopsInHoldOnOther(t)
	setPrimary(t, f, "b", "pg-b")
	setPrimary(t, f, "a", "pg-b")
	sf, err := f.resume("b", engine.Options{AcceptGitHead: headOther})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonStepFailed ||
		!strings.HasPrefix(sf.Status.Message, "step pooler-a: the read before the first Failover step: driver cnpg no longer allows the run: site b is not a replica") {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	for _, s := range sf.Spec.Steps {
		if s.Phase == failoverv1.PhaseFailover && stepState(sf, s.ID) != failoverv1.StepPending {
			t.Errorf("%s is %s: a Failover step ran", s.ID, stepState(sf, s.ID))
		}
	}
	if n := poolerInstances(t, f, "a"); n != int64(2) {
		t.Errorf("Pooler pg-a-pooler-rw has %v instances, want 2", n)
	}
}

// The read changes nothing when the sites are as the plan read them: a
// run stopped in Hold resumes through its switch.
func TestResumeAfterHoldGoesOnWhenTheSitesAreUnchanged(t *testing.T) {
	f := stopsInHoldOnOther(t)
	sf, err := f.resume("b", engine.Options{AcceptGitHead: headOther})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
}
