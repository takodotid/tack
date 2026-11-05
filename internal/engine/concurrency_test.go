package engine_test

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// racedLedger is a ledger that another process writes to while the run
// records step at Running: the other write lands first.
type racedLedger struct {
	engine.Ledger
	step string
	// calls is the number of Flux calls when the other process wrote.
	flux  func() int
	calls int
	raced bool
}

func (l *racedLedger) UpdateStatus(ctx context.Context, sf *failoverv1.SiteFailover) error {
	if !l.raced && stepState(sf, l.step) == failoverv1.StepRunning {
		l.raced = true
		other, err := l.Ledger.Get(ctx, sf.Name)
		if err != nil {
			return err
		}
		other.Status.Message = "written by another tack process"
		if err := l.Ledger.UpdateStatus(ctx, other); err != nil {
			return err
		}
		l.calls = l.flux()
	}
	return l.Ledger.UpdateStatus(ctx, sf)
}

// RFC0003 Requirement 5. Another process writes the ledger while a run
// records demote-a Running. The run's write carries the resourceVersion it
// read, so it fails, and the run ends there: demote-a never runs, and
// nothing else is called.
func TestLedgerWriteConflictEndsTheRun(t *testing.T) {
	f := newFixture(t)
	env := f.env("b")
	raced := &racedLedger{Ledger: env.Ledger, step: "demote-a", flux: func() int { return len(f.flux.Calls) }}
	env.Ledger = raced
	err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{})
	if !raced.raced {
		t.Fatalf("demote-a was never recorded Running: %v", err)
	}
	if !apierrors.IsConflict(err) || !strings.Contains(err.Error(), "another tack process may be running this run") {
		t.Fatalf("err %v, want a conflict on the ledger write", err)
	}
	if n := len(f.flux.Calls); n != raced.calls {
		t.Errorf("%d Flux calls after the failed write: %v", n-raced.calls, f.flux.Calls[raced.calls:])
	}
	if got := field(f.cluster("a"), "spec", "replica", "primary"); got != "pg-a" {
		t.Errorf("site a declares %v: demote-a ran after the failed write", got)
	}
	sf := f.ledger("b")
	if sf.Status.Message != "written by another tack process" || stepState(sf, "demote-a") != failoverv1.StepPending {
		t.Errorf("the ledger has message %q and demote-a %s: the other process's write was replaced", sf.Status.Message, stepState(sf, "demote-a"))
	}
}
