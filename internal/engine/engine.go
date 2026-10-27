// Package engine plans and runs a SiteFailover. It knows no workload: a
// driver supplies the workload's steps, and the engine adds the ledger,
// the Flux hold and release, the Git change and the checks.
package engine

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/kube"
)

// Owner is how Flux applies an object.
type Owner struct {
	// Kustomization applies the object. Nil when no Kustomization does.
	Kustomization *v1alpha1.ObjectKey
	// Helm is true when helm-controller applies the object.
	Helm bool
}

// Flux holds and releases objects and talks to kustomize-controller.
type Flux interface {
	// Owner reads how Flux applies a live object.
	Owner(ctx context.Context, site string, obj v1alpha1.ObjectRef) (Owner, error)

	// Hold stops Flux from applying the object and marks it held by run.
	// It stops the run when another run holds the object, unless that run
	// is takeOver. It returns the Kustomization that applies the object.
	Hold(ctx context.Context, site string, obj v1alpha1.ObjectRef, run, takeOver string) (v1alpha1.ObjectKey, error)

	// Release lets Flux apply the object again, if run holds it. It returns
	// the object's generation.
	Release(ctx context.Context, site string, obj v1alpha1.ObjectRef, run string) (int64, error)

	// Reconcile asks Flux to fetch Git and reconcile the Kustomizations,
	// and waits until each has handled the request. When revision is set,
	// each must have applied it. It returns the revision Flux applied.
	Reconcile(ctx context.Context, site string, ks []v1alpha1.ObjectKey, revision string, timeout time.Duration) (string, error)

	// VerifyGit renders the objects from the repository checked out at dir,
	// as Flux would, and checks that applying them changes nothing live.
	VerifyGit(ctx context.Context, site string, held []v1alpha1.HeldObject, fields []v1alpha1.ObjectField, dir string, strict bool) (string, error)

	// VerifyNoop checks that Flux changed nothing in the released objects.
	VerifyNoop(ctx context.Context, site string, held []v1alpha1.HeldObject, fields []v1alpha1.ObjectField, strict bool) (string, error)
}

// Git reads and writes the GitOps repository with the git binary.
type Git interface {
	// Resolve finds the file and path of each field. When Git cannot be
	// read, it returns the changes unresolved with an error that says so.
	Resolve(ctx context.Context, w *v1alpha1.WorkloadSpec, changes []driver.FieldChange) ([]v1alpha1.GitChange, error)

	// Commit writes one commit on top of base, or adopts the run's commit
	// when it is already there.
	Commit(ctx context.Context, w *v1alpha1.WorkloadSpec, req CommitRequest) (string, error)

	// Checkout returns a directory holding the repository at revision.
	Checkout(ctx context.Context, w *v1alpha1.WorkloadSpec, revision string) (string, error)

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
	Changes []v1alpha1.GitChange
	// Message is the commit message.
	Message string
}

// Routing confirms that routing followed the switch.
type Routing interface {
	// Wait waits until the service routes to the expected member.
	Wait(ctx context.Context, site string, s *v1alpha1.RoutingStep) (string, error)
}

// Ledger stores SiteFailover objects.
type Ledger interface {
	Create(ctx context.Context, sf *v1alpha1.SiteFailover) error
	Get(ctx context.Context, name string) (*v1alpha1.SiteFailover, error)
	// UpdateStatus writes the status. It fails when another writer changed
	// the object since it was read.
	UpdateStatus(ctx context.Context, sf *v1alpha1.SiteFailover) error
	List(ctx context.Context) ([]v1alpha1.SiteFailover, error)
}

// Env is what a run needs.
type Env struct {
	Clients kube.Clients
	Driver  driver.Driver
	Flux    Flux
	Git     Git
	Routing Routing
	Ledger  Ledger
	// Out receives progress lines.
	Out io.Writer
	// Now returns the time. Tests replace it.
	Now func() time.Time
	// Poll is the interval between checks of a wait.
	Poll time.Duration
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
