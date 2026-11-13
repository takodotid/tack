package cnpg

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
)

// The WAL archive a Cluster names, as CloudNativePG v1.30.1 reads it.
func TestArchiveOf(t *testing.T) {
	cluster := func(spec map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
		u.SetName("pg-a")
		return u
	}
	for _, tt := range []struct {
		name string
		spec map[string]any
		want string
	}{
		{"none", map[string]any{}, "none configured"},
		{"in-tree", map[string]any{"backup": map[string]any{"barmanObjectStore": map[string]any{"destinationPath": "s3://wal"}}},
			"barmanObjectStore s3://wal, serverName pg-a"},
		{"in-tree with serverName", map[string]any{"backup": map[string]any{"barmanObjectStore": map[string]any{"destinationPath": "s3://wal", "serverName": "pg-a-2"}}},
			"barmanObjectStore s3://wal, serverName pg-a-2"},
		{"plugin", map[string]any{"plugins": []any{map[string]any{"name": barmanPlugin, "isWALArchiver": true, "parameters": map[string]any{"barmanObjectName": "store-a"}}}},
			"plugin barman-cloud.cloudnative-pg.io, ObjectStore store-a, serverName pg-a"},
		{"plugin that does not archive", map[string]any{"plugins": []any{map[string]any{"name": barmanPlugin, "isWALArchiver": false, "parameters": map[string]any{"barmanObjectName": "store-a"}}}},
			"none configured"},
		{"plugin turned off", map[string]any{"plugins": []any{map[string]any{"name": barmanPlugin, "enabled": false}}}, "none configured"},
	} {
		if got := archiveOf(cluster(tt.spec)).String(); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

// The new primary's timeline is the one its WAL file names.
func TestWalTimeline(t *testing.T) {
	for name, want := range map[string]int32{"000000030000000000000005": 3, "0000000A0000000100000002": 10, "": 0, "000000000000000000000001": 0, "nope": 0} {
		if got, _ := walTimeline(name); got != want {
			t.Errorf("walTimeline(%q) = %d, want %d", name, got, want)
		}
	}
}

// [RFC#0004] Requirement 15: a site loss records the fork point around the
// promotion, and keeps the old primary's archive, or its snapshot when the
// Workload asks for one, after its fence. Legacy plans as v1.6.0 did, so
// its ledgers resume. A planned run records nothing: it forks no history.
func TestRecordSteps(t *testing.T) {
	w := spec3()
	ids := func(r driver.Request) []string {
		steps, _, err := Driver{}.Steps(w, r)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range steps {
			out = append(out, s.ID)
		}
		return out
	}
	loss := driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss}
	got := ids(loss)
	want := []string{"fence-a", "pooler-a", "wait-pooler-a", "record-position-b", "promote-b", "wait-promoted-b", "record-fork-b", "declare-c", "pooler-b", "wait-pooler-b", "record-archive-a", "verify-b", "verify-archiving-b"}
	if !slices.Equal(got, want) {
		t.Errorf("site loss: %v\nwant %v", got, want)
	}
	w.Sites[0].CNPG.Snapshot = true
	got = ids(loss)
	if i := slices.Index(got, "snapshot-a"); i < 0 || got[i+1] != "record-snapshot-a" || slices.Contains(got, "record-archive-a") {
		t.Errorf("site loss with a snapshot: %v", got)
	}
	loss.Legacy = true
	for _, id := range ids(loss) {
		if strings.HasPrefix(id, "record-") || strings.HasPrefix(id, "snapshot-") {
			t.Errorf("a legacy plan has %s", id)
		}
	}
	for _, id := range ids(driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}) {
		if strings.HasPrefix(id, "record-") || strings.HasPrefix(id, "snapshot-") {
			t.Errorf("a planned run has %s", id)
		}
	}
}
