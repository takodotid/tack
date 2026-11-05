package engine_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// After the run's commit, --accept-git-head sets the revision the Release
// phase checks Git against, and keeps the commit's base as it was.
func TestAcceptHeadAfterCommit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	env := f.env("b")
	env.Git = &pushedOver{Git: f.git, by: pushed}
	env.Flux = &headFlux{Flux: f.flux, git: f.git}
	if err := env.Start(ctx, f.plan("b", failoverv1.ModePlanned), engine.Options{}); !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run: %v", err)
	}
	if err := env.Resume(ctx, f.ledger("b"), engine.Options{AcceptGitHead: pushed}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	g := f.ledger("b").Status.Git
	if g.ReleaseRevision != pushed || g.AcceptedRevision != "" || g.Commit == pushed {
		t.Errorf("git status %+v", g)
	}
	if !slices.Contains(f.git.Checkouts, pushed) || slices.Contains(f.git.Checkouts, g.Commit) {
		t.Errorf("checked Git out at %v, want only the accepted head %s", f.git.Checkouts, pushed)
	}
}

// rewritten is the fake Git, except that no commit contains another: the
// branch was rewritten.
type rewritten struct{ *pushedOver }

func (rewritten) Contains(_ context.Context, _ *failoverv1.WorkloadSpec, commit, rev string) (bool, error) {
	return commit == rev, nil
}

// A branch that no longer contains the run's commit stops the run before
// Flux is asked to apply it.
func TestBranchRewrittenAfterCommit(t *testing.T) {
	f := newFixture(t)
	env := f.env("b")
	env.Git = rewritten{&pushedOver{Git: f.git, by: pushed}}
	env.Flux = &headFlux{Flux: f.flux, git: f.git}
	err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{})
	sf := f.ledger("b")
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged || !strings.Contains(sf.Status.Message, "Git was rewritten") {
		t.Fatalf("err %v, %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if st := stepState(sf, "reconcile-git-a"); st != failoverv1.StepFailed {
		t.Errorf("reconcile-git-a is %s", st)
	}
}

// verifyThenRelease runs a planned run whose first release fails, after
// verify-git-a succeeded. The run stops; the release is left to a later
// process.
func verifyThenRelease(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.flux.Fail["Release"] = errors.New("the API server timed out")
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || stepState(sf, "verify-git-a") != failoverv1.StepSucceeded ||
		stepState(sf, "release-a-cluster-pg-a") != failoverv1.StepFailed {
		t.Fatalf("run: %v, verify-git-a %s, release-a-cluster-pg-a %s", err,
			stepState(sf, "verify-git-a"), stepState(sf, "release-a-cluster-pg-a"))
	}
	delete(f.flux.Fail, "Release")
	f.flux.Calls = nil
	return f
}

// #35 row 16 (RFC0005 Requirement 8). A resume checks Git against live
// again before it releases a site, in its own process: the check of an
// earlier process may be hours old. The ledger records the check again.
func TestReleaseChecksGitInItsOwnProcess(t *testing.T) {
	f := verifyThenRelease(t)
	sf, err := f.resume("b", engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s: %s", err, sf.Status.State, sf.Status.Message)
	}
	verify, release := slices.Index(f.flux.Calls, "VerifyGit a"), slices.Index(f.flux.Calls, "Release a pg-a")
	if verify < 0 || release < 0 || verify > release {
		t.Errorf("calls %q: want VerifyGit at site a before its first release", f.flux.Calls)
	}
	for _, st := range sf.Status.Steps {
		if st.ID == "verify-git-a" && (st.Attempts != 2 || st.State != failoverv1.StepSucceeded) {
			t.Errorf("verify-git-a: %s after %d attempts, want Succeeded after 2", st.State, st.Attempts)
		}
		if st.ID == "verify-git-b" && st.Attempts != 1 {
			t.Errorf("verify-git-b ran %d times, want once: it ran in the resume's own process", st.Attempts)
		}
	}
}

// The branch moved between the two processes: the check before the
// release stops the run, and nothing is released.
func TestReleaseChecksGitAfterBranchMoved(t *testing.T) {
	f := verifyThenRelease(t)
	f.git.Head = "d000000000000000000000000000000000000001"
	sf, err := f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged ||
		!strings.HasPrefix(sf.Status.Message, "step verify-git-a: ") {
		t.Fatalf("resume: %v, %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if n := countCalls(f.flux, "Release"); n != 0 {
		t.Errorf("%d releases after the branch moved", n)
	}
}
