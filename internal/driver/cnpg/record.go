package cnpg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/version"
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
	case RecordArchiving:
		if err := cl.Get(ctx, key, u); err != nil {
			return &driver.Recorded{Message: err.Error()}, nil
		}
		ok, msg := archiving(ctx, c, s.Site, u)
		return &driver.Recorded{Done: ok, Message: msg}, nil
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

// RecordArchiving waits until the new primary archives WAL of its own
// timeline ([RFC#0004] Requirement 17).
const RecordArchiving = "Archiving"

// DefaultArchivingTimeout bounds the wait for archiving after a promotion
// when the Workload sets no timeouts.archiving.
const DefaultArchivingTimeout = 5 * time.Minute

// FollowUpCheckArchiving is the follow-up of a new primary that did not
// archive WAL within its timeout.
const FollowUpCheckArchiving = "CheckArchiving"

// archiving tells whether the Cluster's primary archives WAL of the
// timeline it writes: the Cluster's ContinuousArchiving condition is True,
// and the last file it archived, the timeline's history file or a segment
// of it, names that timeline, with no failure since. A replica Cluster
// archives too (archive_mode always), so the condition alone may predate
// the promotion.
func archiving(ctx context.Context, c kube.Clients, site string, u *unstructured.Unstructured) (bool, string) {
	if archiveOf(u).Kind == "" {
		return true, "no WAL archive configured: nothing to check"
	}
	status, msg := condition(u, "ContinuousArchiving")
	if status != "True" {
		return false, fmt.Sprintf("condition ContinuousArchiving is %s (%s)", orNone(status), orNone(msg))
	}
	st, err := readStatus(ctx, c, site, u)
	if err != nil {
		return false, err.Error()
	}
	if !st.IsPrimary {
		return false, "the instance does not report itself primary"
	}
	if len(st.CurrentWAL) < 8 || len(st.LastArchivedWAL) < 8 || st.LastArchivedWAL[:8] != st.CurrentWAL[:8] {
		return false, fmt.Sprintf("the last WAL archived is %q, not yet of the timeline of %q", st.LastArchivedWAL, st.CurrentWAL)
	}
	if !st.IsArchivingWAL {
		return false, fmt.Sprintf("archiving %q failed after %q was archived", st.LastFailedWAL, st.LastArchivedWAL)
	}
	return true, fmt.Sprintf("archives WAL of its timeline: last archived %s", st.LastArchivedWAL)
}

// condition returns a Cluster condition's status and message.
func condition(u *unstructured.Unstructured, kind string) (string, string) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == kind {
			s, _ := m["status"].(string)
			msg, _ := m["message"].(string)
			return s, msg
		}
	}
	return "", ""
}

// Facts of a site's WAL archive.
const (
	// factArchive is where the Cluster's spec says it archives.
	factArchive = "archive"
	// factDestination is the bucket path and server name its WAL goes to,
	// empty when it archives nowhere or cannot be read.
	factDestination = "archiveDestination"
	// factArchiveError says why the destination could not be read.
	factArchiveError = "archiveError"
	// factArchiving is the Cluster's ContinuousArchiving condition.
	factArchiving = "continuousArchiving"
)

// ObjectStoreAPIVersion is the barman-cloud plugin's ObjectStore
// (CloudNativePG v1.30.1, docs/src/recovery.md:72-90).
const ObjectStoreAPIVersion = "barmancloud.cnpg.io/v1"

// readArchive fills a site's archive facts. A plugin's destination is in
// its ObjectStore, which it reads; a read that fails is kept as a fact,
// and the plan notes it.
func readArchive(ctx context.Context, cl client.Client, f map[string]string, u *unstructured.Unstructured) {
	a := archiveOf(u)
	f[factArchive] = a.String()
	status, msg := condition(u, "ContinuousArchiving")
	if status != "" {
		f[factArchiving] = strings.TrimSpace(status + " " + msg)
	}
	switch a.Kind {
	case "":
		return
	case barmanPlugin:
		if a.Store == "" {
			f[factArchiveError] = "the plugin names no barmanObjectName"
			return
		}
		o := &unstructured.Unstructured{}
		o.SetAPIVersion(ObjectStoreAPIVersion)
		o.SetKind("ObjectStore")
		if err := cl.Get(ctx, client.ObjectKey{Namespace: u.GetNamespace(), Name: a.Store}, o); err != nil {
			f[factArchiveError] = fmt.Sprintf("read ObjectStore %s/%s: %v", u.GetNamespace(), a.Store, err)
			return
		}
		a.Path, _, _ = unstructured.NestedString(o.Object, "spec", "configuration", "destinationPath")
	}
	if a.Path == "" {
		f[factArchiveError] = "no destinationPath"
		return
	}
	f[factDestination] = strings.TrimSuffix(a.Path, "/") + "/" + a.Server
}

// sharedArchives returns the pairs of sites that answer whose Clusters
// archive WAL to the same destination: the same path and server name.
func sharedArchives(t *driver.Topology) [][2]driver.SiteState {
	var out [][2]driver.SiteState
	for i, a := range t.Sites {
		for _, b := range t.Sites[i+1:] {
			if a.Reachable && b.Reachable && a.Facts[factDestination] != "" && a.Facts[factDestination] == b.Facts[factDestination] {
				out = append(out, [2]driver.SiteState{a, b})
			}
		}
	}
	return out
}

// sharedMessage says that two sites archive to one destination.
func sharedMessage(p [2]driver.SiteState) string {
	return fmt.Sprintf("sites %s and %s archive WAL to the same destination, %s (Cluster %s: %s; Cluster %s: %s): both write the same WAL file names there, and a site's archive must hold only its own Cluster's history. Give each Cluster its own destination, such as its own serverName (%s)",
		p[0].Name, p[1].Name, p[0].Facts[factDestination], p[0].Facts[factCluster], p[0].Facts[factArchive], p[1].Facts[factCluster], p[1].Facts[factArchive],
		version.DocLink("docs/workload.md#wal-archives"))
}

// archiveNotes are the plan's notes about WAL archives.
func archiveNotes(t *driver.Topology, r driver.Request) []string {
	var notes []string
	configured := false
	for _, s := range t.Sites {
		if !s.Reachable {
			continue
		}
		configured = configured || s.Facts[factDestination] != ""
		if e := s.Facts[factArchiveError]; e != "" {
			notes = append(notes, fmt.Sprintf("warning: site %s: the WAL archive's destination could not be read (%s): the plan cannot check that it is the site's own. Reading a barman-cloud ObjectStore needs get on objectstores (%s)",
				s.Name, e, version.DocLink("docs/operations.md#permissions")))
		}
	}
	if toSt, ok := t.Site(r.To); ok {
		if a := toSt.Facts[factArchiving]; strings.HasPrefix(a, "False") {
			notes = append(notes, fmt.Sprintf("warning: site %s: Cluster %s reports ContinuousArchiving %s: its archiving fails now, and the run waits for it after the promotion, then warns", r.To, toSt.Facts[factCluster], a))
		}
	}
	shared := sharedArchives(t)
	for _, p := range shared {
		if r.Mode == failoverv1.ModeSiteLoss {
			notes = append(notes, "warning: "+sharedMessage(p))
		}
	}
	if configured && len(shared) == 0 {
		notes = append(notes, "WAL archives: each site that answers archives to a destination of its own")
	}
	return notes
}

// StallWithin is how long the wait for a planned promotion lets the new
// primary's replay stand still, short of the demotion token, before it
// stops ([RFC#0004] Requirement 18).
const StallWithin = 2 * time.Minute

// Progress implements driver.Progresser for the wait of a planned
// promotion: the replay position of to's instance, and whether it is still
// short of the demotion token's checkpoint, which CloudNativePG waits for
// before it promotes (v1.30.1, pkg/promotiontoken/promotion_token.go:75-115).
func (Driver) Progress(ctx context.Context, _ *failoverv1.WorkloadSpec, c kube.Clients, s *failoverv1.Step, captured map[string]string) (string, bool) {
	target, ok := tokenLSN(captured[TokenKey])
	if !ok || s.Wait == nil {
		return "", false
	}
	cl, err := c.Client(s.Site)
	if err != nil {
		return "", false
	}
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(s.Wait.Object.APIVersion)
	u.SetKind(s.Wait.Object.Kind)
	if cl.Get(ctx, client.ObjectKey{Namespace: s.Wait.Object.Namespace, Name: s.Wait.Object.Name}, u) != nil {
		return "", false
	}
	st, err := readStatus(ctx, c, s.Site, u)
	if err != nil || st.IsPrimary {
		return "", false
	}
	replay, ok := ParseLSN(st.ReplayLsn)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("site %s's replay at %s, short of %s that the demotion token needs", s.Site, replay, target), replay < target
}

// tokenLSN reads the checkpoint position a demotion token holds: base64 of
// JSON, latestCheckpointREDOLocation (v1.30.1, pkg/utils/parser.go:180-205,
// :237-245).
func tokenLSN(token string) (LSN, bool) {
	b, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return 0, false
	}
	var t struct {
		REDO string `json:"latestCheckpointREDOLocation"`
	}
	if json.Unmarshal(b, &t) != nil {
		return 0, false
	}
	return ParseLSN(t.REDO)
}

// Writes implements driver.Writer: writes stop when the old primary's
// Pooler goes to 0, or when it demotes without one; they come back when the
// new primary's Pooler is ready, or when it runs a healthy primary without
// one. A site loss cannot tell when writes stopped.
func (Driver) Writes(w *failoverv1.WorkloadSpec, r driver.Request) (string, string) {
	from, to := siteByName(w, r.From), siteByName(w, r.To)
	if from == nil || to == nil {
		return "", ""
	}
	resumes := "wait-promoted-" + to.Name
	if to.CNPG.Pooler != nil {
		resumes = "wait-pooler-" + to.Name
	}
	if r.Mode != failoverv1.ModePlanned {
		return "", resumes
	}
	stops := "demote-" + from.Name
	if from.CNPG.Pooler != nil {
		stops = "pooler-" + from.Name
	}
	return stops, resumes
}
