package engine_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// headFlux applies the branch head, as Flux does once it fetched Git,
// whatever revision a step waits for. A wait for another revision times
// out, as a real one does.
type headFlux struct {
	*testenv.Flux
	git *testenv.Git
	// afterVerifyGit runs after the check before release at a site.
	afterVerifyGit func(site string)
}

func (h *headFlux) Reconcile(ctx context.Context, site string, ks []failoverv1.ObjectKey, revision string, fetchGit bool, timeout time.Duration) (string, []string, error) {
	if _, notes, err := h.Flux.Reconcile(ctx, site, ks, revision, fetchGit, timeout); err != nil {
		return "", notes, err
	}
	applied := h.Flux.Revision
	if h.git.Head != "" {
		applied = "main@sha1:" + h.git.Head
	}
	if revision != "" && !engine.SameRevision(applied, revision) {
		return "", nil, fmt.Errorf("timed out after %s waiting for Flux at site %s: applied %s, want %s", timeout, site, applied, revision)
	}
	return applied, nil, nil
}

func (h *headFlux) VerifyGit(ctx context.Context, site string, held []failoverv1.HeldObject, fields []failoverv1.ObjectField, dir string, strict bool) (string, error) {
	msg, err := h.Flux.VerifyGit(ctx, site, held, fields, dir, strict)
	if h.afterVerifyGit != nil {
		h.afterVerifyGit(site)
	}
	return msg, err
}

// pushedOver is the fake Git, except that a commit lands on top of the
// run's commit as soon as the run pushed it.
type pushedOver struct {
	*testenv.Git
	by string
}

func (p *pushedOver) Commit(ctx context.Context, w *failoverv1.WorkloadSpec, req engine.CommitRequest) (string, error) {
	c, err := p.Git.Commit(ctx, w, req)
	if err == nil {
		p.Git.Head = p.by
	}
	return c, err
}

var pushed = "d" + strings.Repeat("0", 39)

// Issue #29. A commit lands on top of the run's commit before Flux at the
// sites applied it. The run must not wait for a commit Flux never applies:
// it stops before the release with GitChanged, and once the operator
// accepts the new head, it releases with the check after release.
func TestPushOnTopOfRunsCommit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	env := f.env("b")
	env.Git = &pushedOver{Git: f.git, by: pushed}
	env.Flux = &headFlux{Flux: f.flux, git: f.git}
	err := env.Start(ctx, f.plan("b", failoverv1.ModePlanned), engine.Options{})
	sf := f.ledger("b")
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged || stepState(sf, "verify-git-a") != failoverv1.StepFailed {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if !strings.Contains(sf.Status.Message, "--accept-git-head "+pushed) {
		t.Errorf("message %q does not give the way on", sf.Status.Message)
	}
	if n := countCalls(f.flux, "Release"); n != 0 {
		t.Fatalf("%d objects released before the operator accepted the new head", n)
	}

	err = env.Resume(ctx, f.ledger("b"), engine.Options{AcceptGitHead: pushed})
	sf = f.ledger("b")
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s: %s", err, sf.Status.State, sf.Status.Message)
	}
	if n := countCalls(f.flux, "VerifyNoop"); n != 2 {
		t.Errorf("the check after release ran %d times, want 2", n)
	}
}

// Issue #29. A commit lands on top of the run's commit after the check
// before release at site a. The release at a must still end with the
// check after release, and site b stops before its release.
func TestPushAfterCheckBeforeRelease(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	env := f.env("b")
	env.Flux = &headFlux{Flux: f.flux, git: f.git, afterVerifyGit: func(site string) {
		if site == "a" {
			f.git.Head = pushed
		}
	}}
	err := env.Start(ctx, f.plan("b", failoverv1.ModePlanned), engine.Options{})
	sf := f.ledger("b")
	if st := stepState(sf, "verify-noop-a"); st != failoverv1.StepSucceeded {
		t.Errorf("verify-noop-a is %s: %s", st, stepMessage(sf, "reconcile-release-a"))
	}
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged || stepState(sf, "verify-git-b") != failoverv1.StepFailed {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
}
