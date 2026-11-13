package cnpg

import (
	"context"
	"fmt"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/kube"
)

// What a Record step records ([RFC#0004] Requirement 15).
const (
	RecordPosition = "Position"
	RecordFork     = "Fork"
	RecordSnapshot = "Snapshot"
	RecordArchive  = "Archive"
)

// DefaultSnapshotTimeout bounds the wait for the old primary's snapshot
// when the Workload sets no timeouts.snapshot.
const DefaultSnapshotTimeout = 30 * time.Minute

// The barman-cloud plugin of CloudNativePG, and its parameters
// (CloudNativePG v1.30.1, docs/src/replica_cluster.md:226-241).
const barmanPlugin = "barman-cloud.cloudnative-pg.io"

// readTries is how often a record reads the instance status before it
// records that it could not.
const readTries = 3

// Record implements driver.Recorder. It only reads. A position or fork
// that cannot be read is recorded as such, and never stops the run: the
// promotion matters more than its record.
func (d Driver) Record(ctx context.Context, w *failoverv1.WorkloadSpec, c kube.Clients, _ string, s *failoverv1.Step) (*driver.Recorded, error) {
	cl, err := c.Client(s.Site)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(s.Record.Object.APIVersion)
	u.SetKind(s.Record.Object.Kind)
	key := client.ObjectKey{Namespace: s.Record.Object.Namespace, Name: s.Record.Object.Name}
	switch s.Record.What {
	case RecordPosition, RecordFork:
		var st *instanceStatus
		var err error
		for i := 0; i < readTries; i++ {
			if err = cl.Get(ctx, key, u); err == nil {
				if st, err = readStatus(ctx, c, s.Site, u); err == nil {
					break
				}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i+1) * time.Second):
			}
		}
		if s.Record.What == RecordPosition {
			return recordPosition(s.Site, st, err), nil
		}
		return recordFork(s.Site, st, err), nil
	case RecordSnapshot:
		if err := cl.Get(ctx, key, u); err != nil {
			return nil, fmt.Errorf("read Backup %s/%s: %w", key.Namespace, key.Name, err)
		}
		return d.recordSnapshot(ctx, w, cl, s, u)
	case RecordArchive:
		if err := cl.Get(ctx, key, u); err != nil {
			return nil, fmt.Errorf("read Cluster %s/%s: %w", key.Namespace, key.Name, err)
		}
		a := archiveOf(u).String()
		return &driver.Recorded{Done: true, Archive: a, Message: "WAL archive: " + a}, nil
	}
	return nil, fmt.Errorf("record %q is not one the cnpg driver knows", s.Record.What)
}

// recordPosition records to's position right before its promotion: its
// new history leaves the old one there, or just after if it replays more
// before it promotes.
func recordPosition(site string, st *instanceStatus, err error) *driver.Recorded {
	f := &failoverv1.ForkPoint{Site: site}
	if err != nil {
		f.Message = "position not read: " + err.Error()
		return &driver.Recorded{Done: true, Fork: f, Message: f.Message + "; the run goes on"}
	}
	l, ok := reach(&driver.Position{Received: st.ReceivedLsn, Replayed: st.ReplayLsn})
	if !ok {
		f.Message = "the instance reported no position"
		return &driver.Recorded{Done: true, Fork: f, Message: f.Message + "; the run goes on"}
	}
	f.Position, f.ParentTimeline = l.String(), int32(st.TimeLineID)
	return &driver.Recorded{Done: true, Fork: f, Message: fmt.Sprintf("site %s reaches %s on timeline %d", site, f.Position, f.ParentTimeline)}
}

// recordFork records the new primary's timeline: the first 8 hexadecimal
// digits of the WAL file it writes, which PostgreSQL names after its
// timeline from the promotion on. The timeline of the last checkpoint
// lags it.
func recordFork(site string, st *instanceStatus, err error) *driver.Recorded {
	now := metav1.Now()
	f := &failoverv1.ForkPoint{Site: site, At: &now}
	switch {
	case err != nil:
		f.Message = "timeline not read: " + err.Error()
	case !st.IsPrimary:
		f.Message = "timeline not read: the instance does not report itself primary"
	default:
		if tl, ok := walTimeline(st.CurrentWAL); ok {
			f.Timeline = tl
			return &driver.Recorded{Done: true, Fork: f, Message: fmt.Sprintf("the new history starts on timeline %d", tl)}
		}
		f.Message = fmt.Sprintf("timeline not read: WAL file name %q", st.CurrentWAL)
	}
	return &driver.Recorded{Done: true, Fork: f, Message: f.Message + "; the run goes on"}
}

// walTimeline reads the timeline in a WAL file's name, such as
// 000000030000000000000005.
func walTimeline(name string) (int32, bool) {
	if len(name) != 24 {
		return 0, false
	}
	n, err := strconv.ParseUint(name[:8], 16, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return int32(n), true
}

// recordSnapshot records the old primary's snapshot once its Backup has
// completed, with the WAL archive its Cluster names. A failed Backup
// stops the run: the operator asked for the snapshot.
func (Driver) recordSnapshot(ctx context.Context, w *failoverv1.WorkloadSpec, cl client.Client, s *failoverv1.Step, b *unstructured.Unstructured) (*driver.Recorded, error) {
	phase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
	switch phase {
	case "completed":
	case "failed", "invalid backup definition":
		msg, _, _ := unstructured.NestedString(b.Object, "status", "error")
		return nil, fmt.Errorf("the Backup %s/%s is %s: %s. Fix the cause (the Cluster needs spec.backup.volumeSnapshot, and the site VolumeSnapshot support), delete the Backup, then resume: the step creates it again", b.GetNamespace(), b.GetName(), phase, orNone(msg))
	default:
		return &driver.Recorded{Message: fmt.Sprintf("Backup %s/%s is %s", b.GetNamespace(), b.GetName(), orNone(phase))}, nil
	}
	rec := &failoverv1.SnapshotRecord{Backup: s.Record.Object}
	now := metav1.Now()
	rec.At = &now
	elements, _, _ := unstructured.NestedSlice(b.Object, "status", "snapshotBackupStatus", "elements")
	for _, e := range elements {
		if m, ok := e.(map[string]any); ok {
			if n, ok := m["name"].(string); ok && n != "" {
				rec.VolumeSnapshots = append(rec.VolumeSnapshots, n)
			}
		}
	}
	out := &driver.Recorded{Done: true, Snapshot: rec,
		Message: fmt.Sprintf("Backup %s/%s completed: VolumeSnapshots %v", b.GetNamespace(), b.GetName(), rec.VolumeSnapshots)}
	site := siteByName(w, s.Site)
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(APIVersion)
	u.SetKind("Cluster")
	if err := cl.Get(ctx, client.ObjectKey{Namespace: site.CNPG.Cluster.Namespace, Name: site.CNPG.Cluster.Name}, u); err != nil {
		return nil, fmt.Errorf("read Cluster %s/%s for its WAL archive: %w", site.CNPG.Cluster.Namespace, site.CNPG.Cluster.Name, err)
	}
	out.Archive = archiveOf(u).String()
	out.Message += "; WAL archive: " + out.Archive
	return out, nil
}

// archive is where a Cluster archives its WAL, as its spec names it.
type archive struct {
	// Kind is barmanObjectStore, the barman-cloud plugin, or empty for none.
	Kind string
	// Path is barmanObjectStore's destinationPath; Store the plugin's
	// ObjectStore, in the Cluster's namespace.
	Path, Store string
	// Server is the server name under the destination: the Cluster's name
	// unless the spec sets serverName.
	Server string
}

// archiveOf reads a Cluster's WAL archive: spec.backup.barmanObjectStore,
// or a barman-cloud plugin that archives WAL (CloudNativePG v1.30.1,
// api/v1/cluster_types.go:2394 and :2596-2618).
func archiveOf(u *unstructured.Unstructured) archive {
	if store, ok, _ := unstructured.NestedMap(u.Object, "spec", "backup", "barmanObjectStore"); ok {
		a := archive{Kind: "barmanObjectStore", Server: u.GetName()}
		a.Path, _ = store["destinationPath"].(string)
		if s, _ := store["serverName"].(string); s != "" {
			a.Server = s
		}
		return a
	}
	plugins, _, _ := unstructured.NestedSlice(u.Object, "spec", "plugins")
	for _, p := range plugins {
		m, ok := p.(map[string]any)
		if !ok || m["name"] != barmanPlugin || m["enabled"] == false || m["isWALArchiver"] == false {
			continue
		}
		params, _ := m["parameters"].(map[string]any)
		a := archive{Kind: barmanPlugin, Server: u.GetName()}
		a.Store, _ = params["barmanObjectName"].(string)
		if s, _ := params["serverName"].(string); s != "" {
			a.Server = s
		}
		return a
	}
	return archive{}
}

func (a archive) String() string {
	switch a.Kind {
	case "":
		return "none configured"
	case barmanPlugin:
		return fmt.Sprintf("plugin %s, ObjectStore %s, serverName %s", a.Kind, orNone(a.Store), a.Server)
	}
	return fmt.Sprintf("barmanObjectStore %s, serverName %s", orNone(a.Path), a.Server)
}
