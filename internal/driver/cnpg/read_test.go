package cnpg

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/takodotid/tack/internal/driver"
)

// cluster builds Cluster pg-a with spec.replica set to replica (none when
// nil), the phase, and one instance reporting isPrimary.
func cluster(replica map[string]any, isPrimary bool, phase string, inProgress bool) *unstructured.Unstructured {
	spec := map[string]any{"instances": int64(1)}
	if replica != nil {
		spec["replica"] = replica
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": spec,
		"status": map[string]any{
			"phase":                      phase,
			"switchReplicaClusterStatus": map[string]any{"inProgress": inProgress},
			"instancesReportedState":     map[string]any{"pg-a-1": map[string]any{"isPrimary": isPrimary}},
		},
	}}
	u.SetAPIVersion(APIVersion)
	u.SetKind("Cluster")
	u.SetNamespace("db")
	u.SetName("pg-a")
	return u
}

// #35 row 42 (RFC0004 Requirement 3): the roles, in the rule's order, and
// health.
func TestRead(t *testing.T) {
	for _, tt := range []struct {
		name       string
		replica    map[string]any
		isPrimary  bool
		phase      string
		inProgress bool
		role       driver.Role
		healthy    bool
	}{
		{name: "declares its name primary", replica: map[string]any{"primary": "pg-a", "source": "pg-b"}, isPrimary: true, phase: PhaseHealthy, role: driver.RolePrimary, healthy: true},
		{name: "declares its self primary", replica: map[string]any{"self": "site-a", "primary": "site-a"}, isPrimary: true, phase: PhaseHealthy, role: driver.RolePrimary, healthy: true},
		{name: "no spec.replica", isPrimary: true, phase: PhaseHealthy, role: driver.RolePrimary, healthy: true},
		{name: "spec.replica without primary", replica: map[string]any{"source": "pg-b"}, isPrimary: true, phase: PhaseHealthy, role: driver.RolePrimary, healthy: true},
		{name: "declares another primary", replica: map[string]any{"primary": "pg-b"}, phase: PhaseHealthy, role: driver.RoleReplica, healthy: true},
		{name: "declares another primary, still reports isPrimary", replica: map[string]any{"primary": "pg-b"}, isPrimary: true, phase: PhaseHealthy, role: driver.RoleReplica, healthy: true},
		{name: "self names another member", replica: map[string]any{"self": "site-a", "primary": "pg-a"}, phase: PhaseHealthy, role: driver.RoleReplica, healthy: true},
		{name: "enabled true", replica: map[string]any{"enabled": true, "primary": "pg-a"}, isPrimary: true, phase: PhaseHealthy, role: driver.RoleUnknown, healthy: true},
		{name: "enabled false", replica: map[string]any{"enabled": false}, isPrimary: true, phase: PhaseHealthy, role: driver.RoleUnknown, healthy: true},
		// #35 row 33, not changed: a Cluster that declares itself primary
		// while no instance reports isPrimary reads Unknown.
		{name: "declares itself primary, no instance isPrimary", replica: map[string]any{"primary": "pg-a"}, phase: PhaseHealthy, role: driver.RoleUnknown, healthy: true},
		{name: "no spec.replica, no instance isPrimary", phase: PhaseHealthy, role: driver.RoleUnknown, healthy: true},
		{name: "unhealthy phase", replica: map[string]any{"primary": "pg-b"}, phase: "Setting up primary", role: driver.RoleReplica},
		{name: "demotion in progress", replica: map[string]any{"primary": "pg-b"}, phase: PhaseHealthy, inProgress: true, role: driver.RoleReplica},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := driver.SiteState{Name: "a", Role: driver.RoleUnknown, Facts: map[string]string{}}
			read(&st, cluster(tt.replica, tt.isPrimary, tt.phase, tt.inProgress), map[string]string{"pg-a": "a", "pg-b": "b"})
			if st.Role != tt.role || st.Healthy != tt.healthy {
				t.Errorf("role %s, healthy %v; want %s, %v", st.Role, st.Healthy, tt.role, tt.healthy)
			}
		})
	}
}
