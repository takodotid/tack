// Package engine plans and runs a SiteFailover. It knows no workload: a
// driver supplies the workload's steps, and the engine adds the ledger,
// the Flux hold and release, the Git change and the checks.
package engine

import (
	"context"
	"errors"
	"io"
	"time"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/kube"
)

// Owner is how Flux applies an object.
type Owner struct {
	// Kustomization applies the object. Nil when no Kustomization does.
	Kustomization *failoverv1.ObjectKey
	// Helm is true when helm-controller applies the object.
	Helm bool
}

// Flux holds and releases objects and talks to kustomize-controller.
type Flux interface {
	// Owner reads how Flux applies a live object.
	Owner(ctx context.Context, site string, obj failoverv1.ObjectRef) (Owner, error)

	// Hold stops Flux from applying the object and marks it held by run.
	// It stops the run when another run holds the object, unless that run
	// is in takeOver. It returns the Kustomization that applies the object.
	Hold(ctx context.Context, site string, obj failoverv1.ObjectRef, run string, takeOver []string) (failoverv1.ObjectKey, error)

	// HeldBy returns the run that holds the object, or "" when it is not
	// held by a run.
	HeldBy(ctx context.Context, site string, obj failoverv1.ObjectRef) (string, error)

	// Release lets Flux apply the object again, if run holds it. Before it
	// removes the hold, it calls record with the object's generation, and
	// keeps the hold when record fails. It returns the object's generation;
	// record was not called when the object was no longer held by run.
	Release(ctx context.Context, site string, obj failoverv1.ObjectRef, run string, record func(generation int64) error) (int64, error)

	// Reconcile asks Flux to reconcile the Kustomizations, and waits until
	// each has handled the request. When fetchGit is set, it first asks
	// their GitRepository to fetch Git. When revision is set, each
	// Kustomization must have applied it. It returns the revision Flux
	// applied, and notes for the ledger, such as a GitRepository that could
	// not fetch.
	Reconcile(ctx context.Context, site string, ks []failoverv1.ObjectKey, revision string, fetchGit bool, timeout time.Duration) (string, []string, error)

	// VerifyGit renders the objects from the repository checked out at dir,
	// as Flux would, and checks that applying them changes nothing live.
	VerifyGit(ctx context.Context, site string, held []failoverv1.HeldObject, fields []failoverv1.ObjectField, dir string, strict bool) (string, error)

	// VerifyNoop checks that Flux changed nothing in the released objects,
	// once their Kustomizations are Ready, waiting up to timeout for that.
	VerifyNoop(ctx context.Context, site string, held []failoverv1.HeldObject, fields []failoverv1.ObjectField, strict bool, timeout time.Duration) (string, error)
}

// Git reads and writes the GitOps repository with the git binary.
type Git interface {
	// Resolve finds the file and path of each field. When Git cannot be
	// read, it returns the changes unresolved with an error that says so.
	Resolve(ctx context.Context, w *failoverv1.WorkloadSpec, changes []driver.FieldChange) ([]failoverv1.GitChange, error)

	// HasMapping reports, without reading Git, whether a change has a
	// spec.git.fields entry, or spec.git.find covers it. Resolve fails
	// with ErrNoMapping on a change without.
	HasMapping(w *failoverv1.WorkloadSpec, c driver.FieldChange) bool

	// Commit writes one commit on top of base, or adopts the run's commit
	// when it is already there.
	Commit(ctx context.Context, w *failoverv1.WorkloadSpec, req CommitRequest) (string, error)

	// Checkout returns a directory holding the repository at revision.
	Checkout(ctx context.Context, w *failoverv1.WorkloadSpec, revision string) (string, error)

	// BranchHead fetches the branch and returns the commit at its head.
	BranchHead(ctx context.Context, w *failoverv1.WorkloadSpec) (string, error)

	// Contains reports whether rev is commit or a descendant of it.
	Contains(ctx context.Context, w *failoverv1.WorkloadSpec, commit, rev string) (bool, error)

	// Close removes local copies.
	Close() error
}

// ErrNoMapping is wrapped by Git.Resolve when a change has neither a field
// mapping nor find enabled.
var ErrNoMapping = errors.New("no mapping for the field")

// CommitRequest is the run's Git change, with captured values substituted.
type CommitRequest struct {
	// Run names the run. The commit carries it as a trailer.
	Run string
	// Base is the commit the branch must be at.
	Base string
	// Changes are the fields to set.
	Changes []failoverv1.GitChange
	// Message is the commit message.
	Message string
}

// FetchesGit tells whether a Reconcile step asks Flux to fetch Git before
// it reconciles. The Hold phase of a SiteLoss run does not: Git may be lost
// with the site, and Hold must not wait on it ([RFC#0007] Requirement 1).
// Flux then applies the artifact it has.
func FetchesGit(mode failoverv1.Mode, phase failoverv1.Phase) bool {
	return !(mode == failoverv1.ModeSiteLoss && phase == failoverv1.PhaseHold)
}

// Routing confirms that routing followed the switch.
type Routing interface {
	// Wait waits until the service routes to the expected member.
	Wait(ctx context.Context, site string, s *failoverv1.RoutingStep) (string, error)
}

// Ledger stores SiteFailover objects.
type Ledger interface {
	Create(ctx context.Context, sf *failoverv1.SiteFailover) error
	Get(ctx context.Context, name string) (*failoverv1.SiteFailover, error)
	// UpdateStatus writes the status. It fails when another writer changed
	// the object since it was read.
	UpdateStatus(ctx context.Context, sf *failoverv1.SiteFailover) error
	List(ctx context.Context) ([]failoverv1.SiteFailover, error)
}

// Recorder is a Ledger that also records Kubernetes Events on a ledger,
// and tells who runs this process at the ledger's site ([RFC#0003]
// Requirement 23). ledger.Store is one.
type Recorder interface {
	// Event records an Event on the ledger sf: a Warning when warning is
	// set, else Normal.
	Event(ctx context.Context, sf *failoverv1.SiteFailover, warning bool, reason, message string, at time.Time) error
	// User returns the user name that the site's API server gives this
	// process's credentials.
	User(ctx context.Context) (string, error)
}

// Env is what a run needs.
type Env struct {
	Clients kube.Clients
	Driver  driver.Driver
	Flux    Flux
	Git     Git
	Routing Routing
	Ledger  Ledger
	// Runs lists the workload's earlier runs at every site that answers c.
	Runs func(ctx context.Context, c kube.Clients) ([]failoverv1.SiteFailover, error)
	// Supersede records in the ledger of run that the run by took it over.
	// Nil does nothing.
	Supersede func(ctx context.Context, run, by string) error
	// StartChecks are the site checks of the search for active runs before
	// Start. Start searches again with them once its ledger exists, so it
	// adds no check of a site. Nil makes new checks.
	StartChecks *SiteChecks
	// Out receives progress lines.
	Out io.Writer
	// Now returns the time. Tests replace it.
	Now func() time.Time
	// Poll is the interval between checks of a wait.
	Poll time.Duration
	// ReachWaits are the pauses between the checks of a site's API before
	// it counts as unreachable. Nil uses DefaultReachWaits.
	ReachWaits []time.Duration

	// proc is what this process records of itself in a ledger's history.
	proc process
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) poll() time.Duration {
	if e.Poll > 0 {
		return e.Poll
	}
	return 2 * time.Second
}
