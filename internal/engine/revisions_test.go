package engine_test

import (
	"errors"
	"strings"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// RFC0003 Requirement 10. Flux at site b applied another revision than at
// site a, where the run took its base. Hold stops with GitChanged before
// any Failover step. Once the operator accepts b's revision, Hold goes on
// and the commit is based on it.
func TestHoldStopsOnAnotherRevisionAtAnotherSite(t *testing.T) {
	f := newFixture(t)
	other := "2222222222222222222222222222222222222222"
	f.flux.Revisions = map[string]string{"b": "main@sha1:" + other}
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged {
		t.Fatalf("err %v, reason %s", err, sf.Status.Reason)
	}
	if !strings.Contains(sf.Status.Message, "Flux at site b applied main@sha1:"+other+", but the run's base is main@sha1:1111111111111111111111111111111111111111") {
		t.Errorf("message %q", sf.Status.Message)
	}
	if st := stepState(sf, "reconcile-hold-b"); st != failoverv1.StepFailed {
		t.Errorf("reconcile-hold-b is %s, want Failed", st)
	}
	for _, s := range sf.Spec.Steps {
		if s.Phase == failoverv1.PhaseFailover && stepState(sf, s.ID) != failoverv1.StepPending {
			t.Errorf("%s is %s: a Failover step ran", s.ID, stepState(sf, s.ID))
		}
	}
	if got := field(f.cluster("a"), "spec", "replica", "primary"); got != "pg-a" {
		t.Errorf("site a declares %v", got)
	}

	sf, err = f.resume("b", engine.Options{AcceptGitHead: other})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume --accept-git-head: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	if len(f.git.Commits) != 1 || f.git.Commits[0].Base != other {
		t.Errorf("commits %d, base %q, want one on %s", len(f.git.Commits), f.git.Commits[0].Base, other)
	}
}
