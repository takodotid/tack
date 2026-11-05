package engine_test

import (
	"errors"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// RFC0007 Requirement 3. A site loss with Git down waits at the Git step:
// the run is Waiting with GitUnreachable, git-commit stays Pending, and no
// step after it runs, so nothing is released.
func TestWaitingForGitRunsNothingAfter(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	f.git.Unreachable = true
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if !errors.Is(err, engine.ErrWaiting) || sf.Status.State != failoverv1.RunWaiting || sf.Status.Reason != failoverv1.ReasonGitUnreachable {
		t.Fatalf("err %v, state %s %s", err, sf.Status.State, sf.Status.Reason)
	}
	if st := stepState(sf, "git-commit"); st != failoverv1.StepPending {
		t.Errorf("git-commit is %s, want Pending", st)
	}
	after := false
	for _, s := range sf.Spec.Steps {
		if s.ID == "git-commit" {
			after = true
			continue
		}
		if st := stepState(sf, s.ID); after && st != failoverv1.StepPending {
			t.Errorf("%s, after the Git step, is %s", s.ID, st)
		}
	}
	for _, name := range []string{"Release", "VerifyGit", "VerifyNoop"} {
		if got := calls(f.flux, name); len(got) > 0 {
			t.Errorf("Flux %s called while the run waits for Git: %v", name, got)
		}
	}
	if len(f.flux.Held) == 0 {
		t.Error("nothing is held while the run waits for Git")
	}
}

// RFC0007 Requirement 4. A site loss waits for Git; meanwhile the branch
// moved from the run's base. Once Git answers, tack resume retries the Git
// step and stops with GitChanged instead of committing on the new head.
func TestResumeAfterWaitingStopsWhenGitChanged(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	f.git.Unreachable = true
	if _, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{}); !errors.Is(err, engine.ErrWaiting) {
		t.Fatalf("run: %v, want waiting", err)
	}
	f.git.Unreachable = false
	f.git.Head = "2222222222222222222222222222222222222222"
	sf, err := f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged {
		t.Fatalf("resume: %v, state %s %s", err, sf.Status.State, sf.Status.Reason)
	}
	if st := stepState(sf, "git-commit"); st != failoverv1.StepFailed || len(f.git.Commits) != 0 {
		t.Errorf("git-commit is %s with %d commits, want Failed with none", st, len(f.git.Commits))
	}
	if got := calls(f.flux, "Release"); len(got) > 0 {
		t.Errorf("released after Git changed: %v", got)
	}
}

// RFC0007 Requirement 5, the read before a site's steps: it checks the
// site it reads up to 3 times, and every other site once, since it does
// not use their state. Site b fails its one check; a's steps still run.
func TestReadChecksOtherSitesOnce(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	if sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{}); err != nil {
		t.Fatalf("run: %v %s", err, sf.Status.Message)
	}
	f.clients.SetDown("a", false)
	before := f.clients.Checks["b"]
	f.clients.Flap("b", false, false, false)
	sf, err := f.resume("b", engine.Options{Fence: []string{"a"}})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	if n := f.clients.Checks["b"] - before; n != 1 {
		t.Errorf("site b checked %d times by the read before a's steps, want 1", n)
	}
	if st := stepState(sf, "fence-a"); st != failoverv1.StepSucceeded {
		t.Errorf("fence-a is %s", st)
	}
}
