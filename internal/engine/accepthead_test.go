package engine_test

import (
	"errors"
	"strings"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

const (
	headOther = "2222222222222222222222222222222222222222"
	headLater = "3333333333333333333333333333333333333333"
	headWrong = "4444444444444444444444444444444444444444"
)

// stopsInHoldOnOther starts a planned run toward b while Flux at b applied
// headOther, not the base of site a: the run stops with GitChanged in
// reconcile-hold-b, before any Failover step.
func stopsInHoldOnOther(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.flux.Revisions = map[string]string{"b": "main@sha1:" + headOther}
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged ||
		!strings.HasPrefix(sf.Status.Message, "step reconcile-hold-b: ") {
		t.Fatalf("err %v, %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	return f
}

// noFailoverStep fails when a Failover step ran, or site a was changed.
func noFailoverStep(t *testing.T, f *fixture, sf *failoverv1.SiteFailover) {
	t.Helper()
	for _, s := range sf.Spec.Steps {
		if s.Phase == failoverv1.PhaseFailover && stepState(sf, s.ID) != failoverv1.StepPending {
			t.Errorf("%s is %s: a Failover step ran", s.ID, stepState(sf, s.ID))
		}
	}
	if got := field(f.cluster("a"), "spec", "replica", "primary"); got != "pg-a" {
		t.Errorf("site a declares %v", got)
	}
}

// #35 row 37. Once the operator accepted a head in Hold, Hold accepted any
// revision at the other sites. A site whose Flux applied yet another
// commit, which the operator never saw, must stop the run again.
func TestHoldComparesWithAcceptedHead(t *testing.T) {
	f := stopsInHoldOnOther(t)
	f.flux.Revisions["b"] = "main@sha1:" + headLater
	sf, err := f.resume("b", engine.Options{AcceptGitHead: headOther})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged ||
		!strings.HasPrefix(sf.Status.Message, "step reconcile-hold-b: Flux at site b applied main@sha1:"+headLater) ||
		!strings.Contains(sf.Status.Message, "--accept-git-head "+headLater) {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	noFailoverStep(t, f, sf)
}

// #35 row 37. --accept-git-head must name the commit the stop named. Any
// other commit is refused before anything is written; HEAD stands for the
// named commit, and becomes the base.
func TestAcceptGitHeadMustNameTheStoppedHead(t *testing.T) {
	f := stopsInHoldOnOther(t)
	before := f.ledger("b").ResourceVersion
	sf, err := f.resume("b", engine.Options{AcceptGitHead: headWrong})
	if err == nil || !strings.Contains(err.Error(), "is not the commit the run stopped on: the stop named "+headOther) {
		t.Fatalf("resume --accept-git-head %s: %v; %s %s", headWrong, err, sf.Status.State, sf.Status.Message)
	}
	if sf.ResourceVersion != before || sf.Status.Git.AcceptedRevision != "" {
		t.Errorf("the ledger was written: resourceVersion %s, then %s; accepted %q", before, sf.ResourceVersion, sf.Status.Git.AcceptedRevision)
	}
	noFailoverStep(t, f, sf)

	sf, err = f.resume("b", engine.Options{AcceptGitHead: "HEAD"})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume --accept-git-head HEAD: %v %s %s", err, sf.Status.State, sf.Status.Message)
	}
	if sf.Status.Git.AcceptedRevision != headOther || len(f.git.Commits) != 1 || f.git.Commits[0].Base != headOther {
		t.Errorf("accepted %q, commits %v; want one on %s", sf.Status.Git.AcceptedRevision, f.git.Commits, headOther)
	}
}

// #35 row 37. The Git step's stop names the branch head: the same rule
// holds there, and an abbreviated commit names it too.
func TestAcceptGitHeadAtTheGitStep(t *testing.T) {
	f := newFixture(t)
	f.git.Head = headOther
	sf, err := f.start("b", failoverv1.ModePlanned, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged || stepState(sf, "git-commit") != failoverv1.StepFailed {
		t.Fatalf("err %v, %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if _, err := f.resume("b", engine.Options{AcceptGitHead: headWrong[:7]}); err == nil || !strings.Contains(err.Error(), "the stop named "+headOther) {
		t.Fatalf("resume --accept-git-head %s: %v", headWrong[:7], err)
	}
	sf, err = f.resume("b", engine.Options{AcceptGitHead: headOther[:7]})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume --accept-git-head %s: %v %s %s", headOther[:7], err, sf.Status.State, sf.Status.Message)
	}
	if f.git.Commits[0].Base != headOther {
		t.Errorf("base %q, want %s", f.git.Commits[0].Base, headOther)
	}
}

// A ledger written by v1.2.x, or kept by its CRD, has no
// status.git.changedRevision: the flag is taken as given, as before. Hold
// still compares with the head it names.
func TestAcceptGitHeadWithoutChangedRevision(t *testing.T) {
	f := stopsInHoldOnOther(t)
	sf := f.ledger("b")
	if sf.Status.Git.ChangedRevision != headOther {
		t.Fatalf("changedRevision %q, want %s", sf.Status.Git.ChangedRevision, headOther)
	}
	sf.Status.Git.ChangedRevision = ""
	if err := f.env("b").Ledger.UpdateStatus(t.Context(), sf); err != nil {
		t.Fatal(err)
	}
	sf, err := f.resume("b", engine.Options{AcceptGitHead: headWrong})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonGitChanged {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if sf.Status.Git.AcceptedRevision != headWrong || sf.Status.Git.ChangedRevision != headOther {
		t.Errorf("accepted %q, changed %q", sf.Status.Git.AcceptedRevision, sf.Status.Git.ChangedRevision)
	}
	noFailoverStep(t, f, sf)
}

// PR #49's finding. After a stop that names no commit, such as FluxChanged,
// the runbook's way on is --accept-git-head HEAD, and the ledger stored
// "HEAD" as the release revision. The check before the next release then
// stopped with GitChanged: "HEAD" names no commit. HEAD now stands for
// the branch head, read when the resume starts ([RFC#0006] Requirement 10).
func TestAcceptGitHeadHEADWithoutNamedCommit(t *testing.T) {
	f := releaseThenDie(t)
	sf, err := f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonFluxChanged || sf.Status.Git.ChangedRevision != "" {
		t.Fatalf("err %v, %s, changed %q: %s", err, sf.Status.Reason, sf.Status.Git.ChangedRevision, sf.Status.Message)
	}
	head := f.git.Head
	sf, err = f.resume("b", engine.Options{AcceptGitHead: engine.AcceptHEAD})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	if got := sf.Status.Git.ReleaseRevision; got != head {
		t.Errorf("release revision %q, want the branch head %s", got, head)
	}
}
