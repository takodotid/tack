package engine

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	failoverv1 "github.com/takodotid/tack/api/v1"
)

// RFC0002 Requirement 4: each Wait operator, and a missing field, which
// reads as empty.
func TestConditionsHold(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"phase":                      "Cluster in healthy state",
			"switchReplicaClusterStatus": map[string]any{"inProgress": true},
			"instancesReportedState":     map[string]any{"pg-1": map[string]any{"isPrimary": false}, "pg-2": map[string]any{"isPrimary": true}},
		},
	}}
	r := &runner{sf: &failoverv1.SiteFailover{Status: failoverv1.SiteFailoverStatus{Captured: map[string]string{"phase": "Cluster in healthy state"}}}}
	phase, missing, primaries := "{.status.phase}", "{.status.demotionToken}", "{.status.instancesReportedState.*.isPrimary}"
	inProgress := "{.status.switchReplicaClusterStatus.inProgress}"
	for _, tt := range []struct {
		path  string
		op    failoverv1.Operator
		value string
		want  bool
	}{
		{phase, failoverv1.OpEquals, "Cluster in healthy state", true},
		{phase, failoverv1.OpEquals, "Setting up primary", false},
		{phase, failoverv1.OpEquals, "$(phase)", true},
		{phase, failoverv1.OpNotEquals, "Setting up primary", true},
		{phase, failoverv1.OpNotEquals, "Cluster in healthy state", false},
		{phase, failoverv1.OpNotEmpty, "", true},
		{phase, failoverv1.OpEmpty, "", false},
		{missing, failoverv1.OpEmpty, "", true},
		{missing, failoverv1.OpNotEmpty, "", false},
		{missing, failoverv1.OpEquals, "", true},
		{missing, failoverv1.OpNotEquals, "true", true},
		{missing, failoverv1.OpNotEquals, "", false},
		{inProgress, failoverv1.OpEmpty, "", false},
		{inProgress, failoverv1.OpNotEquals, "true", false},
		{primaries, failoverv1.OpContains, "true", true},
		{primaries, failoverv1.OpContains, "fals", false},
		{phase, failoverv1.OpContains, "healthy", true},
		{missing, failoverv1.OpContains, "", false},
	} {
		ok, why, err := r.conditionsHold(u, []failoverv1.Condition{{JSONPath: tt.path, Operator: tt.op, Value: tt.value}})
		if err != nil || ok != tt.want {
			t.Errorf("%s %s %q: %v (%s), %v; want %v", tt.path, tt.op, tt.value, ok, why, err, tt.want)
		}
	}
	if _, _, err := r.conditionsHold(u, []failoverv1.Condition{{JSONPath: phase, Operator: "Matches", Value: ".*"}}); err == nil || !strings.Contains(err.Error(), `unknown operator "Matches"`) {
		t.Errorf("unknown operator: %v", err)
	}
	// All conditions must hold.
	if ok, why, _ := r.conditionsHold(u, []failoverv1.Condition{
		{JSONPath: phase, Operator: failoverv1.OpNotEmpty},
		{JSONPath: missing, Operator: failoverv1.OpNotEmpty},
	}); ok || !strings.Contains(why, "{.status.demotionToken}") {
		t.Errorf("one condition of two: %v (%s), want false naming the one that fails", ok, why)
	}
}
