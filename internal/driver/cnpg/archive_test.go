package cnpg

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/testenv"
)

// archiveSites returns three healthy sites, a primary, whose Clusters'
// spec takes what archive returns for each.
func archiveSites(t *testing.T, archive func(site string) map[string]any, extra ...client.Object) (*failoverv1.WorkloadSpec, *driver.Topology) {
	t.Helper()
	objects := map[string][]client.Object{}
	for _, s := range []string{"a", "b", "c"} {
		c := testenv.Cluster(s, "a", s == "a", nil)
		for k, v := range archive(s) {
			c.Object["spec"].(map[string]any)[k] = v
		}
		objects[s] = append([]client.Object{c, testenv.Pooler(s, 0)}, extra...)
	}
	w := spec3()
	topo, err := Driver{}.Discover(context.Background(), w, testenv.NewClients(objects))
	if err != nil {
		t.Fatal(err)
	}
	return w, topo
}

func inTree(path, server string) map[string]any {
	store := map[string]any{"destinationPath": path}
	if server != "" {
		store["serverName"] = server
	}
	return map[string]any{"backup": map[string]any{"barmanObjectStore": store}}
}

// #74 row 2, [RFC#0004] Requirement 16: two sites that archive WAL to the
// same destination, the same path and server name, are refused in a
// planned switchover, and warned of in a site loss. A shared path with
// each Cluster's own server name is no clash.
func TestSharedArchive(t *testing.T) {
	planned := driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}
	loss := driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss}

	w, topo := archiveSites(t, func(s string) map[string]any { return inTree("s3://wal/", "") })
	if _, _, err := (Driver{}).Plan(w, topo, planned); err != nil {
		t.Errorf("each Cluster's own serverName under one path: refused: %v", err)
	}
	if notes := strings.Join(Driver{}.Notes(w, topo, planned), "\n"); !strings.Contains(notes, "WAL archives: each site that answers archives to a destination of its own") {
		t.Errorf("notes %q", notes)
	}

	w, topo = archiveSites(t, func(s string) map[string]any {
		if s == "c" {
			return inTree("s3://wal-c", "")
		}
		return inTree("s3://wal", "pg")
	})
	_, _, err := Driver{}.Plan(w, topo, planned)
	if err == nil || !strings.Contains(err.Error(), "sites a and b archive WAL to the same destination, s3://wal/pg") || !strings.Contains(err.Error(), "docs/workload.md#wal-archives") {
		t.Errorf("planned: %v, want a refusal that names sites a and b", err)
	}
	if _, _, err := (Driver{}).Plan(w, topo, loss); err != nil && strings.Contains(err.Error(), "same destination") {
		t.Errorf("site loss refused: %v", err)
	}
	if notes := strings.Join(Driver{}.Notes(w, topo, loss), "\n"); !strings.Contains(notes, "warning: sites a and b archive WAL to the same destination") {
		t.Errorf("site loss notes %q", notes)
	}
}

// The barman-cloud plugin's destination is in its ObjectStore: two
// ObjectStores of one name at two sites may differ, and two of other names
// may share one destination.
func TestSharedArchiveThroughThePlugin(t *testing.T) {
	plugin := func(store string) map[string]any {
		return map[string]any{"plugins": []any{map[string]any{"name": barmanPlugin, "isWALArchiver": true,
			"parameters": map[string]any{"barmanObjectName": store, "serverName": "pg"}}}}
	}
	store := func(name, path string) *unstructured.Unstructured {
		return testenv.Object(ObjectStoreAPIVersion, "ObjectStore", "db", name, map[string]any{"spec": map[string]any{"configuration": map[string]any{"destinationPath": path}}})
	}
	w, topo := archiveSites(t, func(s string) map[string]any { return plugin("store-" + s) },
		store("store-a", "s3://bucket"), store("store-b", "s3://bucket/"), store("store-c", "s3://other"))
	_, _, err := Driver{}.Plan(w, topo, driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned})
	if err == nil || !strings.Contains(err.Error(), "the same destination, s3://bucket/pg") {
		t.Errorf("err %v, want a refusal for one destination through two ObjectStores", err)
	}

	w, topo = archiveSites(t, func(s string) map[string]any { return plugin("missing") })
	req := driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}
	if _, _, err := (Driver{}).Plan(w, topo, req); err != nil {
		t.Errorf("refused when no ObjectStore can be read: %v", err)
	}
	if notes := strings.Join(Driver{}.Notes(w, topo, req), "\n"); !strings.Contains(notes, "warning: site b: the WAL archive's destination could not be read") || !strings.Contains(notes, "objectstores") {
		t.Errorf("notes %q", notes)
	}
}

// #74 row 1: a plan notes a target whose archiving fails now.
func TestTargetArchivingFails(t *testing.T) {
	objects := map[string][]client.Object{}
	for _, s := range []string{"a", "b", "c"} {
		c := testenv.Cluster(s, "a", s == "a", nil)
		c.Object["spec"].(map[string]any)["backup"] = map[string]any{"barmanObjectStore": map[string]any{"destinationPath": "s3://wal-" + s}}
		if s == "b" {
			c.Object["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "ContinuousArchiving", "status": "False", "message": "unexpected failure invoking barman-cloud-wal-archive"}}
		}
		objects[s] = []client.Object{c, testenv.Pooler(s, 0)}
	}
	w := spec3()
	topo, err := Driver{}.Discover(context.Background(), w, testenv.NewClients(objects))
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(Driver{}.Notes(w, topo, driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}), "\n")
	if !strings.Contains(notes, "warning: site b: Cluster pg-b reports ContinuousArchiving False unexpected failure invoking barman-cloud-wal-archive") {
		t.Errorf("notes %q", notes)
	}
}
