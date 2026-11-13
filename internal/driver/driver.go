// Package driver defines what a workload driver supplies to the engine.
// The engine knows no workload; a driver knows one workload's API.
package driver

import (
	"context"
	"fmt"
	"sort"
	"time"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/kube"
)

// Role is a member's role as the workload itself reports it.
type Role string

const (
	RolePrimary Role = "Primary"
	RoleReplica Role = "Replica"
	RoleUnknown Role = "Unknown"
)

// Request is what the operator asked for.
type Request struct {
	From string
	To   string
	Mode failoverv1.Mode
	// AcceptBehind lets a site loss promote to while another site that
	// answers holds WAL past it ([RFC#0004] Requirement 14).
	AcceptBehind bool
	// Legacy asks for the steps that v1.6.0 and earlier planned, so that a
	// ledger they wrote still resumes ([RFC#0003] Requirement 21).
	Legacy bool
}

// Topology is the workload's state at every site, as read at plan time.
type Topology struct {
	Sites []SiteState
}

// Site returns the named site.
func (t *Topology) Site(name string) (*SiteState, bool) {
	for i := range t.Sites {
		if t.Sites[i].Name == name {
			return &t.Sites[i], true
		}
	}
	return nil, false
}

// SiteState is one site's member.
type SiteState struct {
	Name string
	// Reachable is false when the site's API server did not answer.
	Reachable bool
	// Error is why the site could not be read.
	Error string
	// Role is the member's role, as the workload reports it.
	Role Role
	// DeclaredPrimary is the site this site's spec names as primary.
	DeclaredPrimary string
	// Healthy is the workload's own word that the member is healthy.
	Healthy bool
	// Summary is one line for the operator.
	Summary string
	// Objects are the live objects a run may change at this site.
	Objects []failoverv1.ObjectRef
	// Facts are driver-specific values the driver read, for its own checks.
	Facts map[string]string
	// Created is when the site's member was created. A rebuild recreates it.
	Created time.Time
	// Position is how far the member's copy of the data reaches, nil when
	// the driver reads none. It moves all the time, so the read again
	// after the confirmation does not compare it; the driver's refusals
	// see the new value ([RFC#0009] Requirement 13).
	Position *Position
}

// Position is how far a member's copy of the data reaches, as its
// database reports it.
type Position struct {
	// Received and Replayed are the last WAL positions the member received
	// and replayed, for a replica; Current is the primary's write
	// position. Each is empty when the member does not report it.
	Received, Replayed, Current string
	// Timeline is the member's timeline.
	Timeline int
	// System identifies the database: positions of different systems do
	// not compare.
	System string
	// ReceiverActive is true while the member streams WAL from its source.
	ReceiverActive bool
	// ReplayPaused is true when replay was paused.
	ReplayPaused bool
	// Standing compares the member with the other sites, such as
	// "most caught up" or "behind b by 16 KiB".
	Standing string
	// Error is why the position could not be read.
	Error string
}

// FieldChange is one field of one live object, and its value after the run.
type FieldChange struct {
	Site   string
	Object failoverv1.ObjectRef
	// Field is the path in the object, such as spec.replica.primary.
	Field string
	// Value is the JSON value, or nil to remove the field. It may reference
	// a captured value as $(name).
	Value *string
	// Optional marks a removal that is left out when no mapping or find
	// covers it. The check before release still stops the run if Git sets
	// the field.
	Optional bool
}

// Driver supplies the workload-specific parts of a run.
type Driver interface {
	// Name is the driver's name in the Workload.
	Name() string

	// Validate checks the driver's part of the Workload.
	Validate(w *failoverv1.WorkloadSpec) error

	// Discover reads the workload at every site. An unreachable site is
	// reported in its SiteState, not as an error.
	Discover(ctx context.Context, w *failoverv1.WorkloadSpec, c kube.Clients) (*Topology, error)

	// Plan returns the Failover phase's steps, in order. It checks that the
	// request makes sense for the topology, then returns what Steps returns,
	// with the follow-ups that come from the topology after those of Steps.
	Plan(w *failoverv1.WorkloadSpec, t *Topology, r Request) ([]failoverv1.Step, []failoverv1.FollowUp, error)

	// Steps returns the Failover phase's steps and the follow-ups of a
	// request from the Workload alone, without the checks of Plan. A
	// resume composes the plan again with it ([RFC#0003] Requirement 21).
	Steps(w *failoverv1.WorkloadSpec, r Request) ([]failoverv1.Step, []failoverv1.FollowUp, error)

	// Objects returns the live objects a run may change at a site, from
	// the Workload alone. Discover reports the same in each SiteState.
	Objects(w *failoverv1.WorkloadSpec, site string) []failoverv1.ObjectRef

	// GitChange returns the fields that make Git equal the live state
	// after the run. They depend on the Workload and the request only.
	GitChange(w *failoverv1.WorkloadSpec, t *Topology, r Request) ([]FieldChange, error)

	// Verify returns the conditions that mean the switch is complete at a
	// site, as Wait steps.
	Verify(w *failoverv1.WorkloadSpec, r Request, site string) ([]failoverv1.Step, error)

	// Demotion returns the ID of the Failover step after which site no
	// longer runs as primary: the demotion of from, or its fence in a site
	// loss; at another site but to, the step that declares the new
	// primary there. It returns "" for to. The IDs are the same in the
	// ledgers of every earlier Tack release.
	Demotion(r Request, site string) string
}

// Recorded is what a Record step read.
type Recorded struct {
	// Fork fills the ledger's fork point; set fields replace its own.
	Fork *failoverv1.ForkPoint
	// Snapshot and Archive go on the step site's follow-up Rebuild.
	Snapshot *failoverv1.SnapshotRecord
	Archive  string
	// Message is the step's message.
	Message string
	// Done is false while the record waits for the workload, such as a
	// snapshot not yet complete: the engine asks again until the step's
	// timeout.
	Done bool
}

// Recorder is a Driver that runs Record steps: it reads the workload
// through its own API, and changes nothing ([RFC#0002] Requirement 3).
type Recorder interface {
	Record(ctx context.Context, w *failoverv1.WorkloadSpec, c kube.Clients, run string, s *failoverv1.Step) (*Recorded, error)
}

// Noter is a Driver whose plan says what the operator should know about a
// request that Plan accepts.
type Noter interface {
	// Notes returns the notes of a request that Plan accepted, for the
	// plan to print before the operator approves it.
	Notes(w *failoverv1.WorkloadSpec, t *Topology, r Request) []string
}

var drivers = map[string]Driver{}

// Register adds a built-in driver. Tack never loads a driver from outside.
func Register(d Driver) {
	if _, dup := drivers[d.Name()]; dup {
		panic("driver registered twice: " + d.Name())
	}
	drivers[d.Name()] = d
}

// Get returns the named driver.
func Get(name string) (Driver, error) {
	d, ok := drivers[name]
	if !ok {
		return nil, fmt.Errorf("unknown driver %q (built in: %v)", name, Names())
	}
	return d, nil
}

// Names lists the built-in drivers.
func Names() []string {
	var n []string
	for k := range drivers {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}
