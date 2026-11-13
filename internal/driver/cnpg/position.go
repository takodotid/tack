package cnpg

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

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

// The instance manager's metrics endpoint: CloudNativePG v1.30.1,
// pkg/management/url/url.go (PostgresMetricsPort, PathMetrics). It serves
// it without authentication, over TLS when the postgres container runs
// with --metrics-port-tls (pkg/specs/pods.go).
const (
	MetricsPort = 9187
	MetricsPath = "/metrics"
	// LagMetric is the lag of CloudNativePG's default monitoring query
	// pg_replication (config/manager/default-monitoring.yaml): 0 when the
	// instance is a primary or replayed all the WAL it received, else the
	// seconds since the last transaction it replayed committed.
	LagMetric = "cnpg_pg_replication_lag"
	// ReplayTimeMetric is the column of Tack's replay-time query,
	// deploy/cnpg/replay-time.yaml, which an operator may add to a Cluster
	// (docs/workload.md#replay-time): the commit time, in seconds since
	// 1970, of the last transaction the instance replayed. Commit times
	// are not in WAL order: see committedUpTo. CloudNativePG
	// names a custom metric cnpg_<query>_<column>, and writes NULL as NaN
	// (pkg/management/postgres/utils/utils.go, DBToFloat64).
	ReplayTimeMetric = "cnpg_tack_replay_timestamp_seconds"
)

// now is Tack's clock, for the age of a replay time; tests replace it.
var now = time.Now

// PositionTimeout bounds each read of one site's position: its status,
// then its replay lag.
const PositionTimeout = 5 * time.Second

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
// of the Pod, and a GET of its status through the API server. It returns
// the Pod too.
func readStatus(ctx context.Context, c kube.Clients, site string, u *unstructured.Unstructured) (*instanceStatus, *corev1.Pod, error) {
	p, ok := c.(kube.Proxier)
	if !ok {
		return nil, nil, fmt.Errorf("these clients cannot read a pod through the API server")
	}
	pod, _, _ := unstructured.NestedString(u.Object, "status", "currentPrimary")
	if pod == "" {
		return nil, nil, fmt.Errorf("the Cluster %s/%s names no current primary pod", u.GetNamespace(), u.GetName())
	}
	cl, err := c.Client(site)
	if err != nil {
		return nil, nil, err
	}
	po := &corev1.Pod{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: u.GetNamespace(), Name: pod}, po); err != nil {
		return nil, nil, fmt.Errorf("read Pod %s/%s: %w", u.GetNamespace(), pod, err)
	}
	body, err := p.ProxyGet(ctx, site, u.GetNamespace(), pod, scheme(po), StatusPort, StatusPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read the status of Pod %s/%s through the API server: %w", u.GetNamespace(), pod, err)
	}
	st := &instanceStatus{}
	if err := json.Unmarshal(body, st); err != nil {
		return nil, nil, fmt.Errorf("the status of Pod %s/%s is not what CloudNativePG v1.30 writes: %w", u.GetNamespace(), pod, err)
	}
	return st, po, nil
}

// readMetrics reads a replica's instance manager's metrics, through the
// API server: a GET only.
func readMetrics(ctx context.Context, c kube.Clients, site string, po *corev1.Pod) ([]byte, error) {
	p, ok := c.(kube.Proxier)
	if !ok {
		return nil, fmt.Errorf("these clients cannot read a pod through the API server")
	}
	body, err := p.ProxyGet(ctx, site, po.Namespace, po.Name, metricsScheme(po), MetricsPort, MetricsPath)
	if err != nil {
		return nil, fmt.Errorf("read the metrics of Pod %s/%s through the API server: %w", po.Namespace, po.Name, err)
	}
	return body, nil
}

// metric finds the value of the metric name in metrics of the Prometheus
// text format, whatever its labels.
func metric(body []byte, name string) (string, bool) {
	for line := range strings.Lines(string(body)) {
		n, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || strings.HasPrefix(n, "#") {
			continue
		}
		if n, _, _ = strings.Cut(n, "{"); n != name {
			continue
		}
		value, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
		return value, true
	}
	return "", false
}

// parseLag finds LagMetric in metrics of the Prometheus text format.
func parseLag(body []byte) (time.Duration, error) {
	value, ok := metric(body, LagMetric)
	if !ok {
		return 0, fmt.Errorf("the metrics hold no %s: CloudNativePG exports it with its default monitoring queries, which spec.monitoring.disableDefaultQueries turns off", LagMetric)
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, fmt.Errorf("%s is %q, not a number of seconds", LagMetric, value)
	}
	return time.Duration(f * float64(time.Second)).Round(time.Second), nil
}

// replayTime fills the position's last replayed commit from
// ReplayTimeMetric. Without the metric, it leaves the position as it is.
func replayTime(pos *driver.Position, body []byte) {
	value, ok := metric(body, ReplayTimeMetric)
	if !ok {
		return
	}
	f, err := strconv.ParseFloat(value, 64)
	switch {
	case err == nil && math.IsNaN(f):
		pos.CommitNote = "replayed no transaction since it started"
	case err != nil || math.IsInf(f, 0) || f <= 0:
		pos.CommitNote = fmt.Sprintf("replay time not read: %s is %q, not a time", ReplayTimeMetric, value)
	default:
		t := time.UnixMilli(int64(math.Round(f * 1000))).UTC()
		pos.LastCommit = &t
		pos.CommitAge = now().Sub(t).Truncate(time.Second)
	}
}

// scheme is the status endpoint's scheme, as CloudNativePG finds it:
// https when the postgres container runs with --status-port-tls
// (pkg/management/postgres/webserver/client/remote/instance.go,
// GetStatusSchemeFromPod).
func scheme(p *corev1.Pod) string { return tlsScheme(p, "--status-port-tls") }

// metricsScheme is the metrics endpoint's scheme: https when the postgres
// container runs with --metrics-port-tls, which CloudNativePG adds when
// the Cluster sets spec.monitoring.tls.enabled (pkg/specs/pods.go).
func metricsScheme(p *corev1.Pod) string { return tlsScheme(p, "--metrics-port-tls") }

// tlsScheme is https when the pod's postgres container runs with flag.
func tlsScheme(p *corev1.Pod, flag string) string {
	for _, c := range p.Spec.Containers {
		if c.Name != "postgres" {
			continue
		}
		if slices.Contains(c.Command, flag) {
			return "https"
		}
		break
	}
	return "http"
}

// position reads a site's position. An error is kept in it: a plan never
// fails for a position it cannot read.
func position(ctx context.Context, c kube.Clients, site string, u *unstructured.Unstructured) *driver.Position {
	// A pod that the API server cannot reach must not hold a plan.
	sctx, cancel := context.WithTimeout(ctx, PositionTimeout)
	defer cancel()
	st, po, err := readStatus(sctx, c, site, u)
	if err != nil {
		return &driver.Position{Error: err.Error()}
	}
	pos := &driver.Position{System: st.SystemID, Timeline: st.TimeLineID, ReplayPaused: st.ReplayPaused, ReceiverActive: st.IsWalReceiverActive}
	if st.IsPrimary {
		pos.Current = st.CurrentLsn
		return pos
	}
	pos.Received, pos.Replayed = st.ReceivedLsn, st.ReplayLsn
	lctx, lcancel := context.WithTimeout(ctx, PositionTimeout)
	defer lcancel()
	body, err := readMetrics(lctx, c, site, po)
	if err != nil {
		pos.LagError = err.Error()
		return pos
	}
	if lag, err := parseLag(body); err != nil {
		pos.LagError = err.Error()
	} else {
		pos.ReplayLag = &lag
	}
	replayTime(pos, body)
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
	if tp != nil && tp.Error == "" && tp.LagError != "" {
		notes = append(notes, fmt.Sprintf("site %s: the replay lag could not be read (%s). Tack reads it from the instance manager's metrics, port %d, through the API server, with the same permissions as the position",
			r.To, tp.LagError, MetricsPort))
	}
	if r.Mode == failoverv1.ModeSiteLoss && tp != nil && tp.Error == "" && tp.ReplayLag != nil && *tp.ReplayLag > 0 {
		note := fmt.Sprintf("warning: site %s: the last transaction it replayed committed %s ago, by its clock", r.To, *tp.ReplayLag)
		rcv, okr := ParseLSN(tp.Received)
		rep, okl := ParseLSN(tp.Replayed)
		if okr && okl && rcv > rep {
			note += fmt.Sprintf(". It holds %s of WAL it has not replayed: the promotion replays it before it takes writes, so writes stay down longer", Bytes(uint64(rcv-rep)))
		}
		notes = append(notes, note)
	}
	if r.Mode == failoverv1.ModePlanned {
		if tp != nil && tp.Error == "" && !tp.ReceiverActive {
			notes = append(notes, fmt.Sprintf("site %s does not stream WAL from its source right now: the promotion waits until it has replayed up to the demotion token, for up to %s", r.To, w.Timeouts.Promotion.Duration))
		}
		return notes
	}
	if l, ok := reach(tp); ok {
		notes = append(notes, fmt.Sprintf("site loss: replication between sites is asynchronous. Site %s reaches %s.%s The writes that the old primary took after that are not in it: this run loses them (the RPO), and they stay only at site %s, which the follow-up Rebuild recreates",
			r.To, l, committedUpTo(tp), r.From))
	} else {
		notes = append(notes, fmt.Sprintf("site loss: replication between sites is asynchronous. The writes that the old primary took after the last WAL that site %s received are not in it: this run loses them (the RPO)", r.To))
	}
	if tp != nil && tp.Error == "" && tp.LastCommit == nil && tp.CommitNote != "" {
		notes = append(notes, fmt.Sprintf("site %s: %s, so the plan cannot say when its last replayed commit was", r.To, tp.CommitNote))
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

// committedUpTo says, from the replay-time query, when the last commit
// that a replica replayed was; "" without it. It is no guarantee:
// PostgreSQL stamps a commit before it inserts the commit record
// (xact.c, RecordTransactionCommit), so under concurrent writes a commit
// stamped just before may come later in the WAL, and is not replayed yet.
func committedUpTo(p *driver.Position) string {
	if p.LastCommit == nil {
		return ""
	}
	at := p.LastCommit.Format(driver.CommitTimeLayout)
	when := fmt.Sprintf("%s before this plan, by Tack's clock", p.CommitAge)
	if p.CommitAge < 0 {
		when = fmt.Sprintf("that clock is ahead of Tack's by %s or more", -p.CommitAge)
	}
	return fmt.Sprintf(" Its last replayed commit is from %s, by the old primary's clock (%s). The writes that the old primary committed after it are lost, and so may be a few committed just before it, under concurrent writes.", at, when)
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
