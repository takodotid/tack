package engine_test

import (
	"errors"
	"strings"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// lostSiteReturnsAfterCommit runs a site loss toward b with site a down
// and Git up: the run commits and succeeds with a's steps deferred. Site a
// then comes back, fenced by Flux, which applied rev there.
func (f *fixture) lostSiteReturnsAfterCommit(rev func(commit string) string) *failoverv1.SiteFailover {
	f.t.Helper()
	f.clients.SetDown("a", true)
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded || sf.Status.Git.Commit == "" {
		f.t.Fatalf("run: %v %s %s, commit %q", err, sf.Status.State, sf.Status.Message, sf.Status.Git.Commit)
	}
	if st := stepState(sf, "hold-a-cluster-pg-a"); st != failoverv1.StepDeferred {
		f.t.Fatalf("hold-a-cluster-pg-a is %s, want Deferred", st)
	}
	f.clients.SetDown("a", false)
	setPrimary(f.t, f, "a", "pg-b")
	f.flux.Revision = rev(sf.Status.Git.Commit)
	return sf
}

// #35 row 14 (RFC0007 Design). A lost site comes back after the run's
// commit, and Flux there applied that commit, which fences it. The resume
// holds and releases the site: the run's own commit is not a Git change.
func TestLostSiteReturnsAfterCommit(t *testing.T) {
	f := newFixture(t)
	f.lostSiteReturnsAfterCommit(func(commit string) string { return "main@sha1:" + commit })
	sf, err := f.resume("b", engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	for _, st := range sf.Status.Steps {
		if st.State != failoverv1.StepSucceeded {
			t.Errorf("step %s is %s", st.ID, st.State)
		}
	}
	if sf.Status.Git.BaseRevision != "main@sha1:1111111111111111111111111111111111111111" || sf.Status.Git.AcceptedRevision != "" {
		t.Errorf("git status %+v: the base must stay, and nothing is accepted", sf.Status.Git)
	}
	if len(f.flux.Held) != 0 {
		t.Errorf("objects still held: %v", f.flux.Held)
	}
}

// Flux at the returning site applied a commit pushed on top of the run's
// commit. Hold stops with GitChanged and names it: Hold does not read Git
// ([RFC#0007] Requirement 1). Once the operator accepts that head, the
// resume goes on, and Release checks Git against it.
func TestLostSiteReturnsAfterLaterCommit(t *testing.T) {
	f := newFixture(t)
	later := "d000000000000000000000000000000000000001"
	f.lostSiteReturnsAfterCommit(func(string) string {
		f.git.Head = later
		return "main@sha1:" + later
	})
	sf, err := f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged ||
		!strings.Contains(sf.Status.Message, "--accept-git-head "+later) {
		t.Fatalf("resume: %v, %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if st := stepState(sf, "reconcile-hold-a"); st != failoverv1.StepFailed {
		t.Errorf("reconcile-hold-a is %s, want Failed", st)
	}
	sf, err = f.resume("b", engine.Options{AcceptGitHead: later})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume --accept-git-head: %v %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if g := sf.Status.Git; g.ReleaseRevision != later || g.AcceptedRevision != "" {
		t.Errorf("git status %+v", g)
	}
}

// A revision that is neither the base nor the run's commit still stops the
// run in Hold.
func TestLostSiteReturnsAtAnotherRevision(t *testing.T) {
	f := newFixture(t)
	f.lostSiteReturnsAfterCommit(func(string) string { return "main@sha1:e000000000000000000000000000000000000001" })
	sf, err := f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged {
		t.Fatalf("resume: %v, %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if countCalls(f.flux, "Release") != 2 {
		t.Errorf("%d releases, want only site b's 2 from the first process", countCalls(f.flux, "Release"))
	}
}
