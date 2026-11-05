package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// existing creates a ledger at site with the plan's name, as a run planned
// in the same second left it, with state as its status.
func (f *fixture) existing(site string, state failoverv1.RunState) *failoverv1.SiteFailover {
	f.t.Helper()
	other := f.plan(site, failoverv1.ModePlanned)
	ctx := context.Background()
	l := f.env(site).Ledger
	if err := createDropsStatus(ctx, l, other); err != nil {
		f.t.Fatal(err)
	}
	if state != "" {
		other.Status.State = state
		if err := l.UpdateStatus(ctx, other); err != nil {
			f.t.Fatal(err)
		}
	}
	return other
}

func (f *fixture) ledgerNamed(site, name string) *failoverv1.SiteFailover {
	f.t.Helper()
	sf, err := f.env(site).Ledger.Get(context.Background(), name)
	if err != nil {
		f.t.Fatalf("read ledger %s: %v", name, err)
	}
	return sf
}

// Issue #37. A run planned in the same second as a run that succeeded gets
// the same name. It takes the next free name instead of failing, and its
// commit names it.
func TestRunNameTaken(t *testing.T) {
	f := newFixture(t)
	f.existing("b", failoverv1.RunSucceeded)
	if err := f.env("b").Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	sf := f.ledgerNamed("b", "run-1-2")
	if sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run-1-2 is %s: %s", sf.Status.State, sf.Status.Message)
	}
	if len(f.git.Commits) != 1 {
		t.Fatalf("%d commits, want 1", len(f.git.Commits))
	}
	c := f.git.Commits[0]
	if c.Run != "run-1-2" || !strings.Contains(c.Message, "\n\nRun run-1-2, mode Planned.") {
		t.Errorf("commit of run %q with message %q, want both to name run-1-2", c.Run, c.Message)
	}
	if old := f.ledgerNamed("b", "run-1"); old.Status.State != failoverv1.RunSucceeded || len(old.Status.Steps) != 0 {
		t.Errorf("the earlier run changed: %s, %d steps", old.Status.State, len(old.Status.Steps))
	}
}

// A run that takes over a run with its own name, which never recorded its
// start, takes the next free name. The e2e's TestTakeOver hits this when the
// two runs are planned in one second.
func TestRunNameTakenByRunItTakesOver(t *testing.T) {
	f := newFixture(t)
	f.existing("b", "")
	f.flux.Fail["Hold"] = errors.New("stop at the first hold")
	err := f.env("b").Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{TakeOver: []string{"run-1"}})
	if !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run: %v, want a stop at the first hold", err)
	}
	if sf := f.ledgerNamed("b", "run-1-2"); sf.Spec.TakeOver != "run-1" {
		t.Errorf("run-1-2 takes over %q, want run-1", sf.Spec.TakeOver)
	}
}

// A run with the same name that may still run blocks the new run, as the
// search for active runs does: a second tack process may have created it
// after that search. Nothing is created and nothing changes.
func TestRunNameTakenByActiveRun(t *testing.T) {
	for _, state := range []failoverv1.RunState{"", failoverv1.RunRunning, failoverv1.RunStopped} {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			f.existing("b", state)
			err := f.env("b").Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{})
			if err == nil || !strings.Contains(err.Error(), "the name run-1 is taken") || !strings.Contains(err.Error(), "Nothing was changed") {
				t.Fatalf("run: %v, want a refusal naming run-1", err)
			}
			runs, lerr := f.env("b").Ledger.List(context.Background())
			if lerr != nil || len(runs) != 1 {
				t.Errorf("%d ledgers (%v), want only the earlier one", len(runs), lerr)
			}
			if len(f.flux.Calls) > 0 {
				t.Errorf("the refused run changed something: %v", f.flux.Calls)
			}
		})
	}
}

// After the last name, the run fails without creating anything.
func TestRunNamesAllTaken(t *testing.T) {
	f := newFixture(t)
	f.existing("b", failoverv1.RunSucceeded)
	for n := 2; n <= engine.MaxRunNames; n++ {
		other := f.plan("b", failoverv1.ModePlanned)
		other.Name = "run-1-" + string(rune('0'+n))
		if err := createDropsStatus(context.Background(), f.env("b").Ledger, other); err != nil {
			t.Fatal(err)
		}
		other.Status.State = failoverv1.RunSucceeded
		if err := f.env("b").Ledger.UpdateStatus(context.Background(), other); err != nil {
			t.Fatal(err)
		}
	}
	err := f.env("b").Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{})
	if err == nil || !strings.Contains(err.Error(), "the names run-1 to run-1-9 are all taken") {
		t.Fatalf("run: %v, want a failure naming the names tried", err)
	}
	if len(f.flux.Calls) > 0 {
		t.Errorf("the run changed something: %v", f.flux.Calls)
	}
}

// #35 row 35 (RFC0003 Requirement 7). A name taken by an abandoned run,
// such as one that lost a race of two tack processes, is free for the
// next name: the abandoned run never goes on.
func TestRunNameTakenByAbandonedRun(t *testing.T) {
	f := newFixture(t)
	other := f.existing("b", "")
	other.Status = failoverv1.SiteFailoverStatus{Abandoned: &failoverv1.Abandonment{At: metav1.Now(), Reason: "another run was created at the same time"}}
	if err := f.env("b").Ledger.UpdateStatus(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if err := f.env("b").Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sf := f.ledgerNamed("b", "run-1-2"); sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run-1-2 is %s: %s", sf.Status.State, sf.Status.Message)
	}
}
