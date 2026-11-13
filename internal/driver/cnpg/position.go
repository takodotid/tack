package cnpg

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/version"
)

// The instance manager's status endpoint: CloudNativePG v1.30.1,
// pkg/management/url/url.go (StatusPort, PathPgStatus). It serves it
// without authentication, for the API server's pod proxy
// (pkg/management/postgres/webserver/remote.go).
const (
	StatusPort = 8000
	StatusPath = "/pg/status"
)

// instanceStatus holds the fields of CloudNativePG's PostgresqlStatus
// (pkg/postgres/status.go) that Tack reads.
type instanceStatus struct {
	CurrentLsn          string `json:"currentLsn"`
	ReceivedLsn         string `json:"receivedLsn"`
	ReplayLsn           string `json:"replayLsn"`
	SystemID            string `json:"systemID"`
	IsPrimary           bool   `json:"isPrimary"`
	ReplayPaused        bool   `json:"replayPaused"`
	IsWalReceiverActive bool   `json:"isWalReceiverActive"`
	TimeLineID          int    `json:"timeLineID"`
	CurrentWAL          string `json:"currentWAL"`
	LastArchivedWAL     string `json:"lastArchivedWAL"`
	LastFailedWAL       string `json:"lastFailedWAL"`
	IsArchivingWAL      bool   `json:"isArchivingWAL"`
}

// readStatus reads the instance status of the Cluster's current primary
// pod, the designated primary in a replica Cluster. It only reads: a GET
// of the Pod, and a GET of its status through the API server.
func readStatus(ctx context.Context, c kube.Clients, site string, u *unstructured.Unstructured) (*instanceStatus, error) {
	p, ok := c.(kube.Proxier)
	if !ok {
		return nil, fmt.Errorf("these clients cannot read a pod through the API server")
	}
	pod, _, _ := unstructured.NestedString(u.Object, "status", "currentPrimary")
	if pod == "" {
		return nil, fmt.Errorf("the Cluster %s/%s names no current primary pod", u.GetNamespace(), u.GetName())
	}
	cl, err := c.Client(site)
	if err != nil {
		return nil, err
	}
	po := &corev1.Pod{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: u.GetNamespace(), Name: pod}, po); err != nil {
		return nil, fmt.Errorf("read Pod %s/%s: %w", u.GetNamespace(), pod, err)
	}
	body, err := p.ProxyGet(ctx, site, u.GetNamespace(), pod, scheme(po), StatusPort, StatusPath)
	if err != nil {
		return nil, fmt.Errorf("read the status of Pod %s/%s through the API server: %w", u.GetNamespace(), pod, err)
	}
	st := &instanceStatus{}
	if err := json.Unmarshal(body, st); err != nil {
		return nil, fmt.Errorf("the status of Pod %s/%s is not what CloudNativePG v1.30 writes: %w", u.GetNamespace(), pod, err)
	}
	return st, nil
}

// scheme is the status endpoint's scheme, as CloudNativePG finds it:
// https when the postgres container runs with --status-port-tls
// (pkg/management/postgres/webserver/client/remote/instance.go,
// GetStatusSchemeFromPod).
func scheme(p *corev1.Pod) string {
	for _, c := range p.Spec.Containers {
		if c.Name != "postgres" {
			continue
		}
		if slices.Contains(c.Command, "--status-port-tls") {
			return "https"
		}
		break
	}
	return "http"
}

// position reads a site's position. An error is kept in it: a plan never
// fails for a position it cannot read.
func position(ctx context.Context, c kube.Clients, site string, u *unstructured.Unstructured) *driver.Position {
	st, err := readStatus(ctx, c, site, u)
	if err != nil {
		return &driver.Position{Error: err.Error()}
	}
	pos := &driver.Position{System: st.SystemID, Timeline: st.TimeLineID, ReplayPaused: st.ReplayPaused, ReceiverActive: st.IsWalReceiverActive}
	if st.IsPrimary {
		pos.Current = st.CurrentLsn
	} else {
		pos.Received, pos.Replayed = st.ReceivedLsn, st.ReplayLsn
	}
	return pos
}

// LSN is a WAL position, as PostgreSQL writes it: two hexadecimal numbers
// separated by a slash.
type LSN uint64

// ParseLSN reads a WAL position. It returns false for an empty or bad one.
func ParseLSN(s string) (LSN, bool) {
	hi, lo, ok := strings.Cut(s, "/")
	if !ok {
		return 0, false
	}
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return LSN(h<<32 | l), true
}

// String writes the position as PostgreSQL does.
func (l LSN) String() string { return fmt.Sprintf("%X/%X", uint64(l)>>32, uint64(l)&0xFFFFFFFF) }

// reach is how far a replica's data reaches: the furthest of what it
// received and what it replayed. A promotion replays the WAL a replica
// received before it ends recovery, so received WAL is not lost.
func reach(p *driver.Position) (LSN, bool) {
	if p == nil || p.Error != "" {
		return 0, false
	}
	r, okr := ParseLSN(p.Received)
	l, okl := ParseLSN(p.Replayed)
	switch {
	case okr && okl:
		return max(r, l), true
	case okr:
		return r, true
	case okl:
		return l, true
	}
	return 0, false
}

// stand fills each replica's Standing: the replicas that answer, of one
// database, compared by how far their data reaches.
func stand(t *driver.Topology) {
	system := database(t)
	var best LSN
	var bestSite string
	for _, s := range t.Sites {
		if l, ok := reach(s.Position); ok && s.Role == driver.RoleReplica && s.Position.System == system && (bestSite == "" || l > best) {
			best, bestSite = l, s.Name
		}
	}
	for i := range t.Sites {
		s := &t.Sites[i]
		p := s.Position
		if p == nil || p.Error != "" {
			continue
		}
		if p.Current != "" {
			p.Standing = "primary"
			continue
		}
		l, ok := reach(p)
		switch {
		case !ok:
			p.Standing = "position not reported"
		case s.Role != driver.RoleReplica:
			p.Standing = "not a replica"
		case p.System != system:
			p.Standing = fmt.Sprintf("another database (system %s, not %s): not compared", orNone(p.System), orNone(system))
		case l == best:
			p.Standing = "most caught up"
		default:
			p.Standing = fmt.Sprintf("behind %s by %s", bestSite, Bytes(uint64(best-l)))
		}
	}
}

// Bytes writes a size in bytes for a person.
func Bytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ahead returns the sites that answer, other than from and to, whose
// replica holds WAL past to's, of the same database ([RFC#0004]
// Requirement 14).
func ahead(t *driver.Topology, r driver.Request) []driver.SiteState {
	toSt, ok := t.Site(r.To)
	if !ok {
		return nil
	}
	to, ok := reach(toSt.Position)
	if !ok {
		return nil
	}
	var out []driver.SiteState
	for _, s := range t.Sites {
		if s.Name == r.From || s.Name == r.To || !s.Reachable || s.Role != driver.RoleReplica || s.Position == nil || s.Position.System != toSt.Position.System {
			continue
		}
		if l, ok := reach(s.Position); ok && l > to {
			out = append(out, s)
		}
	}
	return out
}

// aheadOf says how far a site that is ahead reaches past to.
func aheadOf(s, to *driver.SiteState) string {
	l, _ := reach(s.Position)
	m, _ := reach(to.Position)
	return fmt.Sprintf("site %s reaches %s, site %s reaches %s: %s more", s.Name, l, to.Name, m, Bytes(uint64(l-m)))
}

// positionNotes are the notes of a plan about positions.
func positionNotes(w *failoverv1.WorkloadSpec, t *driver.Topology, r driver.Request) []string {
	toSt, ok := t.Site(r.To)
	if !ok {
		return nil
	}
	var notes []string
	perms := version.DocLink("docs/operations.md#permissions")
	for _, s := range t.Sites {
		if s.Reachable && s.Position != nil && s.Position.Error != "" {
			what := "it is not compared with the other sites"
			if s.Name == r.To {
				what = "the plan cannot say how far it reaches, nor compare it with the other sites"
			}
			notes = append(notes, fmt.Sprintf("warning: site %s: the WAL position could not be read (%s): %s. Tack reads it from the instance manager through the API server, which needs get on pods and pods/proxy in the Cluster's namespace (%s)",
				s.Name, s.Position.Error, what, perms))
		}
	}
	tp := toSt.Position
	if tp != nil && tp.Error == "" && tp.ReplayPaused {
		notes = append(notes, fmt.Sprintf("warning: site %s: WAL replay is paused, so its data stops at what it replayed before the pause", r.To))
	}
	if r.Mode == failoverv1.ModePlanned {
		if tp != nil && tp.Error == "" && !tp.ReceiverActive {
			notes = append(notes, fmt.Sprintf("site %s does not stream WAL from its source right now: the promotion waits until it has replayed up to the demotion token, for up to %s", r.To, w.Timeouts.Promotion.Duration))
		}
		return notes
	}
	if l, ok := reach(tp); ok {
		notes = append(notes, fmt.Sprintf("site loss: replication between sites is asynchronous. Site %s reaches %s. The writes that the old primary took after that are not in it: this run loses them (the RPO), and they stay only at site %s, which the follow-up Rebuild recreates",
			r.To, l, r.From))
	} else {
		notes = append(notes, fmt.Sprintf("site loss: replication between sites is asynchronous. The writes that the old primary took after the last WAL that site %s received are not in it: this run loses them (the RPO)", r.To))
	}
	if d := toSt.Facts[factMinApplyDelay]; d != "" {
		notes = append(notes, fmt.Sprintf("warning: site %s: Cluster sets spec.replica.minApplyDelay %s, so it replays WAL that long after it receives it; its replayed position lags what it received", r.To, d))
	}
	for _, s := range ahead(t, r) {
		if r.AcceptBehind {
			notes = append(notes, fmt.Sprintf("warning: --accept-behind: %s. Promoting %s loses what site %s holds past it; the run records a follow-up Rebuild for site %s",
				aheadOf(&s, toSt), r.To, s.Name, s.Name))
		}
	}
	return notes
}

// best is the site, among those ahead of to, whose data reaches furthest.
func best(t *driver.Topology, a []driver.SiteState) string {
	name, top := "", LSN(0)
	for _, s := range a {
		if l, _ := reach(s.Position); name == "" || l > top {
			name, top = s.Name, l
		}
	}
	return name
}

// database is the system the sites compare in: the primary's when one
// answers, else the one most replicas report, the first in the Workload's
// order on a tie.
func database(t *driver.Topology) string {
	count := map[string]int{}
	top := ""
	for _, s := range t.Sites {
		p := s.Position
		if p == nil || p.Error != "" || p.System == "" {
			continue
		}
		if p.Current != "" {
			return p.System
		}
		count[p.System]++
		if top == "" || count[p.System] > count[top] {
			top = p.System
		}
	}
	return top
}
