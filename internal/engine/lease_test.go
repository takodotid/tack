package engine_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/version"
)

// leaseOf reads run-1's Lease at site.
func (f *fixture) leaseOf(site string) *coordinationv1.Lease {
	f.t.Helper()
	l := &coordinationv1.Lease{}
	if err := f.clients.Sites[site].Get(context.Background(), client.ObjectKey{Namespace: "tack-system", Name: "run-1"}, l); err != nil {
		f.t.Fatalf("read the lease: %v", err)
	}
	return l
}

func holder(l *coordinationv1.Lease) string {
	if l.Spec.HolderIdentity == nil {
		return "<nil>"
	}
	return *l.Spec.HolderIdentity
}

// watchedLeases is the ledger of the fixture, which keeps its leases: it
// reads run-1's Lease at each status write, and can make lease writes
// fail or slow.
type watchedLeases struct {
	*ledger.Store
	mu sync.Mutex
	// seen are the Lease's holder and renewal at each status write after
	// the first.
	seen []string
	// writes counts the Lease's updates.
	writes int
	// createErr fails the Lease's create.
	createErr error
	// slow is the pause of each status write.
	slow time.Duration
	// steal makes another process take the Lease at this status write.
	steal int
	n     int
}

func (w *watchedLeases) UpdateStatus(ctx context.Context, sf *failoverv1.SiteFailover) error {
	w.mu.Lock()
	w.n++
	n := w.n
	w.mu.Unlock()
	if n > 1 {
		if l, err := w.Store.Lease(ctx, sf.Name); err == nil {
			w.mu.Lock()
			w.seen = append(w.seen, *l.Spec.HolderIdentity)
			w.mu.Unlock()
			for n == w.steal {
				other := "tack v9.9.9, process 1 on another host"
				l.Spec.HolderIdentity = &other
				err := w.Store.UpdateLease(ctx, l)
				if err == nil {
					break
				}
				if !apierrors.IsConflict(err) {
					return err
				}
				// This process renewed it meanwhile.
				if l, err = w.Store.Lease(ctx, sf.Name); err != nil {
					return err
				}
			}
		}
	}
	time.Sleep(w.slow)
	return w.Store.UpdateStatus(ctx, sf)
}

func (w *watchedLeases) CreateLease(ctx context.Context, l *coordinationv1.Lease) error {
	if w.createErr != nil {
		return w.createErr
	}
	return w.Store.CreateLease(ctx, l)
}

func (w *watchedLeases) UpdateLease(ctx context.Context, l *coordinationv1.Lease) error {
	w.mu.Lock()
	w.writes++
	w.mu.Unlock()
	return w.Store.UpdateLease(ctx, l)
}

// startWatched starts a planned run to b with its ledger in w, and returns
// its progress lines.
func (f *fixture) startWatched(w *watchedLeases, renew time.Duration) (string, error) {
	f.t.Helper()
	var out bytes.Buffer
	env := f.env("b")
	w.Store = env.Ledger.(*ledger.Store)
	env.Ledger, env.Out, env.LeaseRenew = w, &out, renew
	err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{})
	return out.String(), err
}

// #65. A process that runs a run holds the run's Lease, named after the
// run, in the ledger's namespace at its site, owned by the ledger: from
// its first status write to its end, when it marks it released. tack
// abandon and tack run --take-over read it ([RFC#0003] Requirement 28).
func TestRunHoldsItsLease(t *testing.T) {
	f := newFixture(t)
	w := &watchedLeases{}
	if out, err := f.startWatched(w, time.Hour); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	want := "tack " + version.Version + ", process "
	if len(w.seen) < 10 {
		t.Fatalf("the Lease was read at %d writes", len(w.seen))
	}
	for i, h := range w.seen {
		if !strings.HasPrefix(h, want) {
			t.Fatalf("write %d: the Lease's holder is %q, want %q...", i+2, h, want)
		}
	}
	l := f.leaseOf("b")
	sf := f.ledger("b")
	if holder(l) != "" || l.Labels[engine.LabelWorkload] != "pg-main" || len(l.OwnerReferences) != 1 ||
		l.OwnerReferences[0].Kind != "SiteFailover" || l.OwnerReferences[0].Name != "run-1" || l.OwnerReferences[0].UID != sf.UID ||
		*l.Spec.LeaseDurationSeconds != 30 {
		t.Errorf("after the run: holder %q, labels %v, owners %v, duration %d", holder(l), l.Labels, l.OwnerReferences, *l.Spec.LeaseDurationSeconds)
	}
}

// The process renews its Lease every LeaseRenew while it runs.
func TestRunRenewsItsLease(t *testing.T) {
	f := newFixture(t)
	w := &watchedLeases{slow: 20 * time.Millisecond}
	if out, err := f.startWatched(w, 5*time.Millisecond); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if w.writes < 20 {
		t.Errorf("%d writes of the Lease, want renewals every 5ms over a run of 30 slow writes", w.writes)
	}
	if holder(f.leaseOf("b")) != "" {
		t.Errorf("the Lease is still held after the run")
	}
}

// The Lease is best effort: a process that may not write it runs the run,
// and says once that it holds none.
func TestRunWithoutLeaseGoesOn(t *testing.T) {
	f := newFixture(t)
	w := &watchedLeases{createErr: apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "run-1", errors.New("no"))}
	out, err := f.startWatched(w, time.Hour)
	if err != nil || f.ledger("b").Status.State != failoverv1.RunSucceeded {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if n := strings.Count(out, "  note      run run-1 holds no lease (leases.coordination.k8s.io \"run-1\" is forbidden: no): tack abandon and tack run --take-over judge by its ledger"); n != 1 {
		t.Errorf("%d notes of no lease, want 1:\n%s", n, out)
	}
}

// Another process takes the Lease while this one runs the run, such as a
// tack resume of the same run: this process leaves it to that process, and
// says so at its end.
func TestLeaseTakenByAnotherProcess(t *testing.T) {
	f := newFixture(t)
	w := &watchedLeases{steal: 5}
	out, err := f.startWatched(w, time.Millisecond)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if h := holder(f.leaseOf("b")); h != "tack v9.9.9, process 1 on another host" {
		t.Errorf("the Lease's holder is %q, want the other process", h)
	}
	if !strings.Contains(out, "  note      tack v9.9.9, process 1 on another host took run run-1's lease while this process ran it\n") {
		t.Errorf("no note of the lost lease:\n%s", out)
	}
}

// A resume takes over a Lease that another process renewed within its
// duration, such as one killed a moment ago, and names that process.
func TestResumeTakesOverLease(t *testing.T) {
	f := newFixture(t)
	f.diesBeforePromotion(f.plan("b", failoverv1.ModePlanned))
	// The process that died held the run's Lease, renewed a moment ago.
	other := "tack v9.9.9, process 1 on another host"
	now, d := metav1.NewMicroTime(time.Now()), int32(30)
	sf := f.ledger("b")
	if err := f.clients.Sites["b"].Create(context.Background(), &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "tack-system",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "failover.tako.id/v1", Kind: "SiteFailover", Name: "run-1", UID: sf.UID}}},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &other, RenewTime: &now, AcquireTime: &now, LeaseDurationSeconds: &d},
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	env := f.env("b")
	env.Out = &out
	if err := env.Resume(context.Background(), f.ledger("b"), engine.Options{}); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "  note      run run-1's lease was held by "+other+", renewed at ") {
		t.Errorf("no note of the other holder:\n%s", out.String())
	}
	if l := f.leaseOf("b"); holder(l) != "" || l.Spec.LeaseTransitions == nil || *l.Spec.LeaseTransitions < 1 {
		t.Errorf("after the resume: holder %q, transitions %v", holder(l), l.Spec.LeaseTransitions)
	}
}

// fixedLeases answers Lease with one Lease, or an error.
type fixedLeases struct {
	engine.Leaser
	l   *coordinationv1.Lease
	err error
}

func (f fixedLeases) Lease(context.Context, string) (*coordinationv1.Lease, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.l, nil
}

// #65, RFC0003 Requirement 28: when a tack process may still run a run,
// tack abandon and tack run --take-over refuse it. A Lease decides when the
// run has one; a run without, whose ledger an older tack wrote, by its
// ledger: Running, and written within its Running step's timeout and
// StepMargin.
func TestCheckLive(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *metav1.Time { t := metav1.NewTime(now.Add(-d)); return &t }
	run := func(state failoverv1.RunState, running string, last time.Duration) *failoverv1.SiteFailover {
		sf := &failoverv1.SiteFailover{ObjectMeta: metav1.ObjectMeta{Name: "run-1", UID: types.UID("uid-1"), CreationTimestamp: *ago(time.Hour)}}
		sf.Spec.Steps = []failoverv1.Step{
			{ID: "pooler-a", Type: failoverv1.StepPatch, Patch: &failoverv1.PatchStep{}},
			{ID: "wait-pooler-a", Type: failoverv1.StepWait, Wait: &failoverv1.WaitStep{Timeout: metav1.Duration{Duration: 10 * time.Minute}}},
		}
		sf.Status.State, sf.Status.WrittenBy = state, "v1.4.0"
		sf.Status.Steps = []failoverv1.StepStatus{{ID: "pooler-a", State: failoverv1.StepSucceeded, StartedAt: ago(last), FinishedAt: ago(last)}, {ID: "wait-pooler-a", State: failoverv1.StepPending}}
		if running != "" {
			sf.Status.Steps[1].State, sf.Status.Steps[1].StartedAt, sf.Status.Steps[0].FinishedAt = failoverv1.StepRunning, ago(last), ago(last+time.Second)
		}
		return sf
	}
	lease := func(by string, renewed time.Duration, uid string) *coordinationv1.Lease {
		r, d := metav1.NewMicroTime(now.Add(-renewed)), int32(30)
		return &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{Kind: "SiteFailover", Name: "run-1", UID: types.UID(uid)}}},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &by, RenewTime: &r, LeaseDurationSeconds: &d},
		}
	}
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "run-1")
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "run-1", errors.New("no"))
	for _, tt := range []struct {
		name  string
		store engine.Leaser
		sf    *failoverv1.SiteFailover
		// refuse is a part of the refusal, "" when CheckLive lets the run be marked.
		refuse, note string
	}{
		{name: "lease renewed 5s ago", store: fixedLeases{l: lease("tack v1.5.0, process 7 on host-a", 5*time.Second, "uid-1")}, sf: run(failoverv1.RunStopped, "", time.Hour),
			refuse: "run run-1 is run by tack v1.5.0, process 7 on host-a, whose lease on it was renewed at 2026-11-11T09:59:55Z and holds until 2026-11-11T10:00:35Z"},
		{name: "lease renewed 39s ago, within its 30s and the skew", store: fixedLeases{l: lease("p", 39*time.Second, "uid-1")}, sf: run(failoverv1.RunRunning, "wait-pooler-a", time.Second), refuse: "is run by p"},
		{name: "lease expired", store: fixedLeases{l: lease("p", 41*time.Second, "uid-1")}, sf: run(failoverv1.RunRunning, "wait-pooler-a", time.Second)},
		{name: "lease released", store: fixedLeases{l: lease("", time.Second, "uid-1")}, sf: run(failoverv1.RunRunning, "wait-pooler-a", time.Second)},
		{name: "lease of another ledger of the name", store: fixedLeases{l: lease("p", time.Second, "uid-0")}, sf: run(failoverv1.RunRunning, "wait-pooler-a", 2*time.Minute),
			note: "the lease named run-1 is not run run-1's", refuse: "in step wait-pooler-a, until 2026-11-11T10:13:00Z"},
		{name: "no lease, Running in a Wait of 10m started 2m ago", store: fixedLeases{err: notFound}, sf: run(failoverv1.RunRunning, "wait-pooler-a", 2*time.Minute),
			refuse: "run run-1 is Running with no lease that tells, and its ledger was last written at 2026-11-11T09:58:00Z by tack v1.4.0: a tack process may still run it, in step wait-pooler-a, until 2026-11-11T10:13:00Z"},
		{name: "no lease, Running in a Wait of 10m started 16m ago", store: fixedLeases{err: notFound}, sf: run(failoverv1.RunRunning, "wait-pooler-a", 16*time.Minute)},
		{name: "no lease, Running between steps, written 4m ago", store: fixedLeases{err: notFound}, sf: run(failoverv1.RunRunning, "", 4*time.Minute), refuse: "between steps, until 2026-11-11T10:01:00Z"},
		{name: "no lease, Running between steps, written 6m ago", store: fixedLeases{err: notFound}, sf: run(failoverv1.RunRunning, "", 6*time.Minute)},
		{name: "no lease, Stopped a moment ago", store: fixedLeases{err: notFound}, sf: run(failoverv1.RunStopped, "", time.Second)},
		{name: "lease that cannot be read, Running", store: fixedLeases{err: forbidden}, sf: run(failoverv1.RunRunning, "wait-pooler-a", time.Minute),
			note: "run run-1's lease could not be read (leases.coordination.k8s.io \"run-1\" is forbidden: no)", refuse: "with no lease that tells"},
		{name: "no store, Running", sf: run(failoverv1.RunRunning, "", time.Minute), refuse: "with no lease that tells"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			note, err := engine.CheckLive(context.Background(), tt.store, tt.sf, now)
			if note != tt.note {
				t.Errorf("note %q, want %q", note, tt.note)
			}
			switch {
			case tt.refuse == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tt.refuse != "" && (!errors.Is(err, engine.ErrMayRun) || !strings.Contains(err.Error(), tt.refuse)):
				t.Errorf("err %v, want a refusal with %q", err, tt.refuse)
			}
		})
	}
}
