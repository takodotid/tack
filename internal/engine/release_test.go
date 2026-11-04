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
