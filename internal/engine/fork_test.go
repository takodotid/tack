package engine_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// stepOf returns a step's status.
func stepOf(sf *failoverv1.SiteFailover, id string) failoverv1.StepStatus {
	for _, s := range sf.Status.Steps {
		if s.ID == id {
			return s
		}
	}
	return failoverv1.StepStatus{}
}

// rebuildOf returns the follow-up Rebuild of a site.
func rebuildOf(t *testing.T, sf *failoverv1.SiteFailover, site string) failoverv1.FollowUp {
	t.Helper()
	for _, f := range sf.Status.FollowUps {
		if f.Site == site && f.Action == engine.FollowUpRebuild {
			return f
		}
	}
	t.Fatalf("no Rebuild for site %s: %+v", site, sf.Status.FollowUps)
	return failoverv1.FollowUp{}
}

// #73 row 1, [RFC#0004] Requirement 15: a site loss records where the new
// primary's history leaves the old one's: its position right before the
// promotion, the timeline it was on, and its new timeline.
func TestSiteLossRecordsTheFork(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	fork := sf.Status.Fork
	if fork == nil || fork.Site != "b" || fork.Position != "0/3000060" || fork.ParentTimeline != 1 || fork.Timeline != 2 || fork.At == nil || fork.Message != "" {
		t.Fatalf("fork %+v, want site b at 0/3000060 on timeline 1, new timeline 2", fork)
	}
	if got := stepOf(sf, "record-archive-a").State; got != failoverv1.StepDeferred {
		t.Errorf("record-archive-a: %s, want Deferred while site a is lost", got)
	}

	// Site a answers again, still primary: the operator approves its fence,
	// and its archive goes on its Rebuild.
	f.clients.SetDown("a", false)
	sf, err = f.resume("b", engine.Options{Fence: []string{"a"}})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if a := rebuildOf(t, sf, "a").Archive; a != "none configured" {
		t.Errorf("archive %q, want none configured", a)
	}
}

// A fork point that cannot be read is recorded as such, and the run goes
// on: the promotion matters more than its record.
func TestForkNotReadGoesOn(t *testing.T) {
	f := newFixture(t)
	f.clients.SetDown("a", true)
	f.clients.SetStatus("b", nil, errors.New("pods \"pg-b-1\" is forbidden"))
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if st := sf.Status.State; st != failoverv1.RunSucceeded {
		t.Fatalf("run %s, want Succeeded", st)
	}
	fork := sf.Status.Fork
	if fork == nil || fork.Position != "" || !strings.Contains(fork.Message, "position not read") || !strings.Contains(fork.Message, "timeline not read") {
		t.Errorf("fork %+v, want both reads named as failed", fork)
	}
	if st := stepOf(sf, "record-position-b"); st.State != failoverv1.StepSucceeded || !strings.Contains(st.Message, "the run goes on") {
		t.Errorf("record-position-b: %+v", st)
	}
}

// #73 row 1, item 2, [RFC#0004] Requirement 15: with the Workload's
// snapshot, the run takes a cold volumeSnapshot Backup of the old primary
// once its site answers, after its fence, and records it and the old
// archive on the old site's Rebuild.
func TestSiteLossSnapshotsTheOldPrimary(t *testing.T) {
	f := newFixture(t)
	f.w.Spec.Sites[0].CNPG.Snapshot = true
	a := f.cluster("a")
	u := &unstructured.Unstructured{Object: a}
	_ = unstructured.SetNestedField(u.Object, map[string]any{"destinationPath": "s3://wal/site-a"}, "spec", "backup", "barmanObjectStore")
	if err := f.clients.Sites["a"].Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	f.clients.SetDown("a", true)
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, id := range []string{"snapshot-a", "record-snapshot-a"} {
		if got := stepOf(sf, id).State; got != failoverv1.StepDeferred {
			t.Errorf("%s: %s, want Deferred", id, got)
		}
	}

	f.clients.SetDown("a", false)
	sf, err = f.resume("b", engine.Options{Fence: []string{"a"}})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	b := testenv.Object("postgresql.cnpg.io/v1", "Backup", "db", "run-1-a", nil)
	if err := f.clients.Sites["a"].Get(context.Background(), client.ObjectKeyFromObject(b), b); err != nil {
		t.Fatalf("no Backup run-1-a at site a: %v", err)
	}
	spec, _, _ := unstructured.NestedMap(b.Object, "spec")
	if spec["method"] != "volumeSnapshot" || spec["online"] != false || spec["target"] != "primary" || b.GetLabels()[engine.LabelRun] != "run-1" {
		t.Errorf("Backup spec %v, labels %v", spec, b.GetLabels())
	}
	if primary, _, _ := unstructured.NestedString(f.cluster("a"), "spec", "replica", "primary"); primary != "pg-b" {
		t.Errorf("pg-a declares %s primary: the fence did not run", primary)
	}
	ids := []string{}
	for _, s := range sf.Spec.Steps {
		ids = append(ids, s.ID)
	}
	if slices.Index(ids, "fence-a") > slices.Index(ids, "snapshot-a") {
		t.Errorf("the snapshot comes before the fence: %v", ids)
	}
	rb := rebuildOf(t, sf, "a")
	if rb.Snapshot == nil || rb.Snapshot.Backup.Name != "run-1-a" || !slices.Equal(rb.Snapshot.VolumeSnapshots, []string{"run-1-a"}) || rb.Snapshot.At == nil {
		t.Errorf("snapshot %+v", rb.Snapshot)
	}
	if rb.Archive != "barmanObjectStore s3://wal/site-a, serverName pg-a" {
		t.Errorf("archive %q", rb.Archive)
	}
}

// The operator asked for the snapshot: a Backup that fails stops the run,
// and says what to fix.
func TestFailedSnapshotStopsTheRun(t *testing.T) {
	f := newFixture(t)
	f.w.Spec.Sites[0].CNPG.Snapshot = true
	testenv.BackupFails = "no volumeSnapshot section defined on the target cluster"
	t.Cleanup(func() { testenv.BackupFails = "" })
	f.clients.SetDown("a", true)
	if _, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	f.clients.SetDown("a", false)
	sf, err := f.resume("b", engine.Options{Fence: []string{"a"}})
	if !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("resume: %v, want a stop", err)
	}
	st := stepOf(sf, "record-snapshot-a")
	if st.State != failoverv1.StepFailed || !strings.Contains(st.Message, "no volumeSnapshot section") || !strings.Contains(st.Message, "spec.backup.volumeSnapshot") {
		t.Errorf("record-snapshot-a: %+v", st)
	}
}

// A Create step adopts only the object its own run created: an object of
// the name that another run, or a person, made stops the run.
func TestCreateAdoptsOnlyItsOwnObject(t *testing.T) {
	f := newFixture(t)
	f.w.Spec.Sites[0].CNPG.Snapshot = true
	other := testenv.Object("postgresql.cnpg.io/v1", "Backup", "db", "run-1-a", map[string]any{"spec": map[string]any{"cluster": map[string]any{"name": "pg-a"}}})
	other.SetLabels(map[string]string{engine.LabelRun: "another-run"})
	if err := f.clients.Sites["a"].Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	f.clients.SetDown("a", true)
	if _, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	f.clients.SetDown("a", false)
	sf, err := f.resume("b", engine.Options{Fence: []string{"a"}})
	if !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("resume: %v, want a stop", err)
	}
	if st := stepOf(sf, "snapshot-a"); st.State != failoverv1.StepFailed || !strings.Contains(st.Message, "this run did not create it") {
		t.Errorf("snapshot-a: %+v", st)
	}
}
