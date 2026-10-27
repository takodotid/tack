package testenv

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Cluster returns a CloudNativePG Cluster named pg-<site> in namespace db
// that declares pg-<primary> primary. isPrimary sets its instance's report.
func Cluster(site, primary string, isPrimary bool, extra map[string]any) *unstructured.Unstructured {
	replica := map[string]any{"self": "pg-" + site, "primary": "pg-" + primary, "source": "pg-" + other(site)}
	for k, v := range extra {
		replica[k] = v
	}
	return Object("postgresql.cnpg.io/v1", "Cluster", "db", "pg-"+site, map[string]any{
		"spec": map[string]any{"instances": int64(1), "replica": replica},
		"status": map[string]any{
			"phase":          "Cluster in healthy state",
			"currentPrimary": "pg-" + site + "-1",
			"timelineID":     int64(1),
			"instancesReportedState": map[string]any{
				"pg-" + site + "-1": map[string]any{"isPrimary": isPrimary, "timeLineID": int64(1)},
			},
		},
	})
}

// Pooler returns the read-write Pooler pg-<site>-rw with n instances.
func Pooler(site string, n int64) *unstructured.Unstructured {
	return Object("postgresql.cnpg.io/v1", "Pooler", "db", "pg-"+site+"-rw", map[string]any{
		"spec": map[string]any{"cluster": map[string]any{"name": "pg-" + site}, "instances": n, "type": "rw"},
	})
}

// Site returns the objects of a healthy site.
func Site(site, primary string, poolers int64) []client.Object {
	return []client.Object{Cluster(site, primary, site == primary, nil), Pooler(site, poolers)}
}

func other(site string) string {
	if site == "a" {
		return "b"
	}
	return "a"
}
