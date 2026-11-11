package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/config"
	"github.com/takodotid/tack/internal/version"
)

// Leaser is a Ledger that also keeps a Lease per run, at the ledger's site
// and in its namespace: the process that runs the run holds it ([RFC#0003]
// Requirement 28). ledger.Store is one.
type Leaser interface {
	// Lease returns the run's Lease, or an error that
	// apierrors.IsNotFound accepts when it has none.
	Lease(ctx context.Context, run string) (*coordinationv1.Lease, error)
	CreateLease(ctx context.Context, l *coordinationv1.Lease) error
	// UpdateLease fails when another writer changed the Lease since it was
	// read.
	UpdateLease(ctx context.Context, l *coordinationv1.Lease) error
}

const (
	// LeaseDuration is how long a run's Lease holds after its last
	// renewal.
	LeaseDuration = 30 * time.Second
	// LeaseRenew is the pause between renewals.
	LeaseRenew = 10 * time.Second
	// LeaseSkew is the margin for the difference between the clock of the
	// host that renewed a Lease and the clock of the host that reads it.
	LeaseSkew = 10 * time.Second
	// StepMargin is what a step may take beyond its own timeout, such as
	// its git commands, and the ledger write and Event after it.
	StepMargin = 5 * time.Minute
	// leaseWrite bounds each write of a Lease, as an Event is bounded.
	leaseWrite = 5 * time.Second
)

// leaseHolder names this process in a Lease: its tack, process and host.
func leaseHolder() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "an unknown host"
	}
	return fmt.Sprintf("tack %s, process %d on %s", version.Version, os.Getpid(), host)
}

// heldLease is the Lease this process holds on a run, and its renewal.
type heldLease struct {
	env    *Env
	store  Leaser
	run    string
	holder string
	stop   chan struct{}
	done   chan struct{}

	mu sync.Mutex
	l  *coordinationv1.Lease
	// failed is the first renewal that failed; lost names the holder that
	// took the Lease.
	failed error
	lost   string
}

// holdLease takes run sf's Lease for this process, and renews it every
// LeaseRenew until the returned release, which marks it released. The
// Lease is best effort: a run whose Lease cannot be written goes on,
// with a progress line that says so. A Lease that another process
// renewed within its duration is taken over with a line that names that
// process: the ledger's writes stop one of two writers ([RFC#0003]
// Requirement 5). Nothing but release writes to the progress lines after
// it returns.
func (e *Env) holdLease(ctx context.Context, sf *failoverv1.SiteFailover) (release func()) {
	store, ok := e.Ledger.(Leaser)
	if !ok {
		return func() {}
	}
	h := &heldLease{env: e, store: store, run: sf.Name, holder: leaseHolder(), stop: make(chan struct{}), done: make(chan struct{})}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseWrite)
	defer cancel()
	now := metav1.NewMicroTime(e.now())
	seconds := int32(LeaseDuration / time.Second)
	l, err := store.Lease(wctx, sf.Name)
	if err == nil {
		if by := holderOf(l); by != "" && fresh(l, e.now()) {
			e.printf("  note      run %s's lease was held by %s, renewed at %s: that process may still run it, and the ledger's writes stop one of the two\n",
				sf.Name, by, l.Spec.RenewTime.UTC().Format(time.RFC3339))
		}
		if holderOf(l) != h.holder {
			n := int32(1)
			if l.Spec.LeaseTransitions != nil {
				n = *l.Spec.LeaseTransitions + 1
			}
			l.Spec.LeaseTransitions = &n
		}
		if !ownedBy(l, sf) {
			// The Lease of a deleted ledger of the same name, or one
			// whose owner was removed: this run owns it from now on, or
			// the garbage collector deletes it while the run runs, and
			// CheckLive does not count it as this run's.
			ownBy(l, sf)
		}
		l.Spec.HolderIdentity, l.Spec.LeaseDurationSeconds, l.Spec.AcquireTime, l.Spec.RenewTime = &h.holder, &seconds, &now, &now
		// The garbage collector may delete such a Lease between the read
		// and the write: this run then creates its own.
		err = store.UpdateLease(wctx, l)
	}
	if apierrors.IsNotFound(err) {
		l = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: sf.Name, Namespace: sf.Namespace},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &h.holder, LeaseDurationSeconds: &seconds, AcquireTime: &now, RenewTime: &now},
		}
		ownBy(l, sf)
		err = store.CreateLease(wctx, l)
	}
	if err != nil {
		e.printf("  note      run %s holds no lease (%v): tack abandon and tack run --take-over judge by its ledger whether a tack process runs it\n", sf.Name, err)
		return func() {}
	}
	h.l = l
	go h.renew(e.leaseRenew())
	return h.release
}

func (e *Env) leaseRenew() time.Duration {
	if e.LeaseRenew > 0 {
		return e.LeaseRenew
	}
	return LeaseRenew
}

// renew renews the Lease every interval until stop, or until another
// process took it. It records what failed, for release to print.
func (h *heldLease) renew(every time.Duration) {
	defer close(h.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		}
		if !h.write(func(l *coordinationv1.Lease) {
			now := metav1.NewMicroTime(h.env.now())
			l.Spec.RenewTime = &now
		}) {
			return
		}
	}
}

// write changes a copy of the Lease with change and writes it. On a
// conflict it reads the Lease again, and writes once more while this
// process still holds it. It returns false once another process holds it.
func (h *heldLease) write(change func(*coordinationv1.Lease)) bool {
	ctx, cancel := context.WithTimeout(context.Background(), leaseWrite)
	defer cancel()
	h.mu.Lock()
	l := h.l.DeepCopy()
	h.mu.Unlock()
	for try := 0; ; try++ {
		change(l)
		err := h.store.UpdateLease(ctx, l)
		if err == nil {
			h.mu.Lock()
			h.l = l
			h.mu.Unlock()
			return true
		}
		if !apierrors.IsConflict(err) || try > 0 {
			h.fail(err)
			return true
		}
		got, gerr := h.store.Lease(ctx, h.run)
		if gerr != nil {
			h.fail(gerr)
			return true
		}
		if by := holderOf(got); by != h.holder {
			h.mu.Lock()
			h.lost = by
			h.mu.Unlock()
			return false
		}
		l = got
	}
}

func (h *heldLease) fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed == nil {
		h.failed = err
	}
}

// release stops the renewal, and marks the Lease released, with no holder,
// unless another process took it. A release that fails leaves the Lease to
// expire after LeaseDuration.
func (h *heldLease) release() {
	close(h.stop)
	<-h.done
	h.mu.Lock()
	lost := h.lost
	h.mu.Unlock()
	if lost == "" {
		h.write(func(l *coordinationv1.Lease) {
			none, now := "", metav1.NewMicroTime(h.env.now())
			l.Spec.HolderIdentity, l.Spec.RenewTime = &none, &now
		})
	}
	h.mu.Lock()
	failed, lost := h.failed, h.lost
	h.mu.Unlock()
	switch {
	case lost != "":
		h.env.printf("  note      %s took run %s's lease while this process ran it\n", lost, h.run)
	case failed != nil:
		h.env.printf("  note      a write of run %s's lease failed (%v): it may have expired while this process ran the run\n", h.run, failed)
	}
}

// holderOf returns the holder a Lease names, "" when none.
func holderOf(l *coordinationv1.Lease) string {
	if l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}

// fresh tells whether a Lease was renewed within its duration, plus
// LeaseSkew, of now.
func fresh(l *coordinationv1.Lease, now time.Time) bool {
	return now.Before(expiry(l))
}

// expiry is when a Lease stops holding, as a reader with a clock up to
// LeaseSkew behind the holder's sees it.
func expiry(l *coordinationv1.Lease) time.Time {
	if l.Spec.RenewTime == nil {
		return time.Time{}
	}
	d := LeaseDuration
	if l.Spec.LeaseDurationSeconds != nil {
		d = time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
	}
	return l.Spec.RenewTime.Add(d + LeaseSkew)
}

// ErrMayRun is wrapped by CheckLive's refusal.
var ErrMayRun = errors.New("a tack process may still run the run")

// CheckLive refuses to mark or take over run sf while a tack process may
// still run it ([RFC#0003] Requirement 28; issue #65): tack abandon does
// not stop that process, and its step in flight can land after a new run
// started. A process holds the run's Lease, which store reads at the
// ledger's site: one that names a holder and was renewed within its
// duration and LeaseSkew refuses. A Lease that names no holder, or
// expired, lets the run be marked. A run without a Lease, whose ledger a
// tack before the Lease wrote, or one that could not write it, is judged
// by its ledger: a Running run refuses until its Running step's timeout
// and StepMargin passed since the newest time its ledger records. note
// says why the Lease was not used, when it could not be read.
func CheckLive(ctx context.Context, store Leaser, sf *failoverv1.SiteFailover, now time.Time) (note string, err error) {
	if store != nil {
		l, err := store.Lease(ctx, sf.Name)
		switch {
		case err == nil && !ownedBy(l, sf):
			note = fmt.Sprintf("the lease named %s is not run %s's", sf.Name, sf.Name)
		case err == nil:
			by := holderOf(l)
			if by == "" || !fresh(l, now) {
				return "", nil
			}
			return "", fmt.Errorf("%w: run %s is run by %s, whose lease on it was renewed at %s and holds until %s. Stop that process, or wait until then: tack abandon does not stop it, and its step in flight can land after a new run starts",
				ErrMayRun, sf.Name, by, l.Spec.RenewTime.UTC().Format(time.RFC3339), expiry(l).UTC().Format(time.RFC3339))
		case !apierrors.IsNotFound(err):
			note = fmt.Sprintf("run %s's lease could not be read (%v)", sf.Name, err)
		}
	}
	if sf.Status.State != failoverv1.RunRunning {
		return note, nil
	}
	last, step := lastWrite(sf)
	until := last.Add(stepBound(sf, step) + StepMargin)
	if !now.Before(until) {
		return note, nil
	}
	in := "between steps"
	if step != nil {
		in = "in step " + step.ID
	}
	return note, fmt.Errorf("%w: run %s is Running with no lease that tells, and its ledger was last written at %s by tack %s: a tack process may still run it, %s, until %s. Check that no tack process runs it, then wait until then: tack abandon does not stop one, and its step in flight can land after a new run starts",
		ErrMayRun, sf.Name, last.UTC().Format(time.RFC3339), orUnknown(sf.Status.WrittenBy), in, until.UTC().Format(time.RFC3339))
}

// ownedBy tells whether a Lease is run sf's: its owner is sf's ledger.
// The Lease of a deleted ledger of the same name is not.
func ownedBy(l *coordinationv1.Lease, sf *failoverv1.SiteFailover) bool {
	for _, o := range l.OwnerReferences {
		if o.Kind == "SiteFailover" && o.Name == sf.Name && o.UID == sf.UID {
			return true
		}
	}
	return false
}

// ownBy makes a Lease run sf's: sf's ledger is its one SiteFailover owner,
// and it carries sf's workload label. An owner of another kind stays.
func ownBy(l *coordinationv1.Lease, sf *failoverv1.SiteFailover) {
	owners := []metav1.OwnerReference{{APIVersion: failoverv1.GroupVersion.String(), Kind: "SiteFailover", Name: sf.Name, UID: sf.UID}}
	for _, o := range l.OwnerReferences {
		if o.Kind != "SiteFailover" {
			owners = append(owners, o)
		}
	}
	l.OwnerReferences = owners
	if l.Labels == nil {
		l.Labels = map[string]string{}
	}
	l.Labels[LabelWorkload] = sf.Spec.Workload.Name
}

// lastWrite returns the newest time sf's ledger records, and its step
// recorded Running, if any.
func lastWrite(sf *failoverv1.SiteFailover) (time.Time, *failoverv1.Step) {
	var last time.Time
	later := func(t *metav1.Time) {
		if t != nil && t.After(last) {
			last = t.Time
		}
	}
	later(sf.Status.StartedAt)
	later(&sf.CreationTimestamp)
	for i := range sf.Status.History {
		later(&sf.Status.History[i].Time)
	}
	var running *failoverv1.Step
	for i := range sf.Status.Steps {
		st := &sf.Status.Steps[i]
		later(st.StartedAt)
		later(st.FinishedAt)
		if st.State == failoverv1.StepRunning {
			for j := range sf.Spec.Steps {
				if sf.Spec.Steps[j].ID == st.ID {
					running = &sf.Spec.Steps[j]
				}
			}
		}
	}
	return last, running
}

// stepBound is the timeout of a step: a Wait's, a Reconcile's or a
// Routing's own, the Workload's Flux timeout for the steps that wait on
// Flux without one, and none for the others.
func stepBound(sf *failoverv1.SiteFailover, s *failoverv1.Step) time.Duration {
	if s == nil {
		return 0
	}
	switch {
	case s.Wait != nil:
		return s.Wait.Timeout.Duration
	case s.Reconcile != nil:
		return s.Reconcile.Timeout.Duration
	case s.Routing != nil:
		return s.Routing.Timeout.Duration
	case s.Type == failoverv1.StepRelease, s.Type == failoverv1.StepVerifyGit, s.Type == failoverv1.StepVerifyNoop:
		if t := sf.Spec.Workload.Spec.Timeouts.Flux; t != nil && t.Duration > 0 {
			return t.Duration
		}
		return config.DefaultFlux
	}
	return 0
}
