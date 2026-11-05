package engine_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"syscall"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

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

// flakyLedger fails one ledger write, the one that records step Running,
// with err. When landed is set, the write reaches the API server first,
// and only its answer is lost.
type flakyLedger struct {
	engine.Ledger
	step   string
	err    error
	landed bool
	failed bool
	writes int
}

func (l *flakyLedger) UpdateStatus(ctx context.Context, sf *failoverv1.SiteFailover) error {
	l.writes++
	if l.failed || stepState(sf, l.step) != failoverv1.StepRunning {
		return l.Ledger.UpdateStatus(ctx, sf)
	}
	l.failed = true
	if l.landed {
		if err := l.Ledger.UpdateStatus(ctx, sf.DeepCopy()); err != nil {
			return err
		}
	}
	return l.err
}

// noAnswer is the error of a request that got no answer.
var noAnswer = &url.Error{Op: "Put", URL: "https://site-b:6443/apis/failover.tako.id/v1/namespaces/tack-system/sitefailovers/run-1/status", Err: syscall.ECONNRESET}

// #35 row 32 (RFC0003 Requirement 5). A ledger write that gets no answer,
// or a 503, is repeated with the same resourceVersion, and the run goes
// on. One whose answer was lost after it landed conflicts on the repeat:
// the ledger holds the status sent, so the run goes on too.
func TestLedgerWriteRetried(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		landed bool
	}{
		{name: "no answer", err: noAnswer},
		{name: "503", err: apierrors.NewServiceUnavailable("etcd leader changed")},
		{name: "landed, answer lost", err: noAnswer, landed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			env := f.env("b")
			l := &flakyLedger{Ledger: env.Ledger, step: "demote-a", err: tt.err, landed: tt.landed}
			env.Ledger = l
			if err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{}); err != nil {
				t.Fatalf("run: %v", err)
			}
			if !l.failed {
				t.Fatal("no write failed")
			}
			if sf := f.ledger("b"); sf.Status.State != failoverv1.RunSucceeded || stepState(sf, "demote-a") != failoverv1.StepSucceeded {
				t.Errorf("%s, demote-a %s", sf.Status.State, stepState(sf, "demote-a"))
			}
		})
	}
}

// #35 row 32. A write the API server refuses for a reason other than a
// conflict ends the run at once, without a repeat, and its message names
// the reason, not another tack process.
func TestLedgerWriteRefused(t *testing.T) {
	gr := schema.GroupResource{Group: "failover.tako.id", Resource: "sitefailovers"}
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{name: "forbidden", err: apierrors.NewForbidden(gr, "run-1", errors.New("no update on sitefailovers/status")), want: "the operator may not update sitefailovers/status"},
		{name: "not found", err: apierrors.NewNotFound(gr, "run-1"), want: "run run-1 is no longer in namespace tack-system"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			env := f.env("b")
			l := &flakyLedger{Ledger: env.Ledger, step: "demote-a", err: tt.err}
			env.Ledger = l
			err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{})
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "another tack process") {
				t.Fatalf("err %v, want %q", err, tt.want)
			}
			writes := l.writes
			if got := field(f.cluster("a"), "spec", "replica", "primary"); got != "pg-a" {
				t.Errorf("site a declares %v: demote-a ran after the failed write", got)
			}
			if sf := f.ledger("b"); stepState(sf, "demote-a") != failoverv1.StepPending {
				t.Errorf("demote-a is %s", stepState(sf, "demote-a"))
			}
			if l.writes != writes {
				t.Errorf("%d writes after the refused one", l.writes-writes)
			}
		})
	}
}
