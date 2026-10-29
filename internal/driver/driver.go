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
	// request makes sense for the topology.
	Plan(w *failoverv1.WorkloadSpec, t *Topology, r Request) ([]failoverv1.Step, []failoverv1.FollowUp, error)

	// GitChange returns the fields that make Git equal the live state
	// after the run.
	GitChange(w *failoverv1.WorkloadSpec, t *Topology, r Request) ([]FieldChange, error)

	// Verify returns the conditions that mean the switch is complete at a
	// site, as Wait steps.
	Verify(w *failoverv1.WorkloadSpec, r Request, site string) ([]failoverv1.Step, error)
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
