package cnpg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// replicaAt is the instance status of a replica that reached lsn.
func replicaAt(lsn string) map[string]any {
	return map[string]any{"systemID": "7300000000000000001", "timeLineID": 1, "isPrimary": false,
		"receivedLsn": lsn, "replayLsn": lsn, "isWalReceiverActive": false}
}

// threeSites returns the clients of three sites: a primary at a when
// aPrimary, else a fenced by hand; b and c replicas of a.
func threeSites(aPrimary bool) *testenv.Clients {
	objects := map[string][]client.Object{}
	for _, s := range []string{"a", "b", "c"} {
		objects[s] = []client.Object{testenv.Cluster(s, "a", s == "a" && aPrimary, nil), testenv.Pooler(s, 0)}
	}
	return testenv.NewClients(objects)
}

func discover(t *testing.T, c *testenv.Clients) (*failoverv1.WorkloadSpec, *driver.Topology) {
	t.Helper()
	w := spec3()
	topo, err := Driver{}.Discover(context.Background(), w, c)
	if err != nil {
		t.Fatal(err)
	}
	return w, topo
}

// RFC0004 Requirement 13: each site's position, read through the API
// server's pod proxy, and how the replicas compare.
func TestPositions(t *testing.T) {
	c := threeSites(true)
	c.SetStatus("b", replicaAt("0/3000060"), nil)
	c.SetStatus("c", replicaAt("0/3000100"), nil)
	_, topo := discover(t, c)
	for site, want := range map[string]string{"a": "primary", "b": "behind c by 160 B", "c": "most caught up"} {
		s, _ := topo.Site(site)
		if s.Position == nil || s.Position.Standing != want {
			t.Errorf("site %s: position %+v, want standing %q", site, s.Position, want)
		}
	}
	if b, _ := topo.Site("b"); b.Position.Received != "0/3000060" || b.Position.Replayed != "0/3000060" || b.Position.Timeline != 1 {
		t.Errorf("site b: %+v", b.Position)
	}
	if a, _ := topo.Site("a"); a.Position.Current != "0/3000060" {
		t.Errorf("site a: %+v", a.Position)
	}
	// A GET of the status per site, then a GET of the metrics per replica.
	var got []string
	for _, p := range c.ProxyLog() {
		if p.Namespace != "db" || p.Pod != "pg-"+p.Site+"-1" {
			t.Errorf("proxied %+v, want pod pg-<site>-1", p)
		}
		got = append(got, fmt.Sprintf("%s %s:%d%s", p.Site, p.Scheme, p.Port, p.Path))
	}
	want := "a https:8000/pg/status, b https:8000/pg/status, b http:9187/metrics, c https:8000/pg/status, c http:9187/metrics"
	if strings.Join(got, ", ") != want {
		t.Errorf("proxied %s, want %s", strings.Join(got, ", "), want)
	}
	if b, _ := topo.Site("b"); b.Position.ReplayLag == nil || *b.Position.ReplayLag != 0 || b.Position.LagError != "" {
		t.Errorf("site b: replay lag %v, %q, want 0", b.Position.ReplayLag, b.Position.LagError)
	}
	if a, _ := topo.Site("a"); a.Position.ReplayLag != nil || a.Position.LagError != "" {
		t.Errorf("the primary's replay lag was read: %v, %q", a.Position.ReplayLag, a.Position.LagError)
	}
}

// RFC0004 Requirement 13: a replica's replay lag, from its instance
// manager's metrics, with the scheme its postgres container serves them.
func TestReplayLag(t *testing.T) {
	c := threeSites(false)
	c.SetDown("a", true)
	c.SetStatus("b", map[string]any{"systemID": "7300000000000000001", "timeLineID": 1, "receivedLsn": "0/5000000", "replayLsn": "0/3000000"}, nil)
	c.SetMetrics("b", `# HELP cnpg_pg_replication_lag Replication lag behind primary in seconds
# TYPE cnpg_pg_replication_lag gauge
cnpg_pg_replication_lag 130.4
cnpg_pg_replication_in_recovery 1
`, nil)
	c.SetMetrics("c", "cnpg_pg_postmaster_start_time 1.7e+09\n", nil)
	w, topo := discover(t, c)
	b, _ := topo.Site("b")
	if b.Position.ReplayLag == nil || *b.Position.ReplayLag != 130*time.Second {
		t.Errorf("site b: replay lag %v, want 2m10s", b.Position.ReplayLag)
	}
	cs, _ := topo.Site("c")
	if cs.Position.ReplayLag != nil || !strings.Contains(cs.Position.LagError, "spec.monitoring.disableDefaultQueries") {
		t.Errorf("site c: replay lag %v, %q, want an error that names disableDefaultQueries", cs.Position.ReplayLag, cs.Position.LagError)
	}
	if cs.Position.Error != "" || cs.Position.Received == "" {
		t.Errorf("site c: a replay lag that cannot be read loses the position: %+v", cs.Position)
	}

	notes := strings.Join(Driver{}.Notes(w, topo, driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss, AcceptBehind: true}), "\n")
	want := "warning: site b: the last transaction it replayed committed 2m10s ago, by its clock. It holds 32.0 MiB of WAL it has not replayed: the promotion replays it before it takes writes, so writes stay down longer"
	if !strings.Contains(notes, want) {
		t.Errorf("notes %q do not say %q", notes, want)
	}
	planned := strings.Join(Driver{}.Notes(w, topo, driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}), "\n")
	if strings.Contains(planned, "committed") {
		t.Errorf("a planned plan warns of the replay lag: %q", planned)
	}
	toC := strings.Join(Driver{}.Notes(w, topo, driver.Request{From: "a", To: "c", Mode: failoverv1.ModeSiteLoss}), "\n")
	if !strings.Contains(toC, "site c: the replay lag could not be read (the metrics hold no cnpg_pg_replication_lag") || strings.Contains(toC, "warning: site c: the replay lag") {
		t.Errorf("notes %q do not say, without a warning, that c's replay lag could not be read", toC)
	}

	// A lag of 0 never warns.
	c.SetMetrics("b", "", nil)
	w, topo = discover(t, c)
	if notes := strings.Join(Driver{}.Notes(w, topo, driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss, AcceptBehind: true}), "\n"); strings.Contains(notes, "committed") {
		t.Errorf("a replay lag of 0 warns: %q", notes)
	}

	// A metrics read that fails keeps the position.
	c.SetMetrics("b", "", errors.New(`pods "pg-b-1" is forbidden`))
	_, topo = discover(t, c)
	if b, _ := topo.Site("b"); b.Position.Error != "" || b.Position.Received != "0/5000000" || !strings.Contains(b.Position.LagError, "forbidden") {
		t.Errorf("site b: %+v", b.Position)
	}
}

// The metrics endpoint's scheme is CloudNativePG's: https only with
// --metrics-port-tls.
func TestMetricsScheme(t *testing.T) {
	p := testenv.Pod("db", "pg-a-1")
	if got := metricsScheme(p); got != "http" {
		t.Errorf("scheme %s, want http", got)
	}
	p.Spec.Containers[0].Command = append(p.Spec.Containers[0].Command, "--metrics-port-tls")
	if got := metricsScheme(p); got != "https" {
		t.Errorf("scheme %s, want https", got)
	}
}

// parseLag reads the one metric, whatever its labels, and nothing else.
func TestParseLag(t *testing.T) {
	for _, tt := range []struct {
		body string
		want time.Duration
		err  string
	}{
		{"cnpg_pg_replication_lag 0\n", 0, ""},
		{"# HELP cnpg_pg_replication_lag x\ncnpg_pg_replication_lag_other 9\ncnpg_pg_replication_lag 1.5e+01\n", 15 * time.Second, ""},
		{`cnpg_pg_replication_lag{instance="pg-b-1"} 61.6 1700000000000` + "\n", 62 * time.Second, ""},
		{"cnpg_pg_replication_lag NaN\n", 0, "not a number of seconds"},
		{"cnpg_pg_replication_lag -1\n", 0, "not a number of seconds"},
		{"", 0, "hold no cnpg_pg_replication_lag"},
	} {
		got, err := parseLag([]byte(tt.body))
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("parseLag(%q) = %v, %v, want an error with %q", tt.body, got, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("parseLag(%q) = %v, %v, want %v", tt.body, got, err, tt.want)
		}
	}
}

// The scheme is CloudNativePG's: https only with --status-port-tls.
func TestStatusScheme(t *testing.T) {
	p := testenv.Pod("db", "pg-a-1")
	if got := scheme(p); got != "https" {
		t.Errorf("scheme %s, want https", got)
	}
	p.Spec.Containers = []corev1.Container{{Name: "postgres", Command: []string{"/controller/manager", "instance", "run"}}}
	if got := scheme(p); got != "http" {
		t.Errorf("scheme %s, want http", got)
	}
}

// A replica's reach is the furthest of what it received and replayed.
func TestReach(t *testing.T) {
	for _, tt := range []struct {
		received, replayed, want string
	}{
		{"0/3000100", "0/3000060", "0/3000100"},
		{"", "1/0", "1/0"},
		{"16/B374D848", "", "16/B374D848"},
	} {
		l, ok := reach(&driver.Position{Received: tt.received, Replayed: tt.replayed})
		if !ok || l.String() != tt.want {
			t.Errorf("reach(%s, %s) = %s, %v, want %s", tt.received, tt.replayed, l, ok, tt.want)
		}
	}
	if _, ok := reach(&driver.Position{}); ok {
		t.Error("a position without LSNs reaches somewhere")
	}
}

// RFC0004 Requirement 14: a site loss toward a replica behind another is
// refused, with a message that names both sites, the most caught-up site
// and the override. With --accept-behind it goes on, warns, and records
// a Rebuild for the site that is ahead.
func TestSiteLossRefusesTargetBehind(t *testing.T) {
	c := threeSites(false)
	c.SetDown("a", true)
	c.SetStatus("b", replicaAt("0/3000060"), nil)
	c.SetStatus("c", replicaAt("0/5A01C2F0"), nil)
	w, topo := discover(t, c)
	req := driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss}
	_, _, err := Driver{}.Plan(w, topo, req)
	if err == nil {
		t.Fatal("a site loss toward a replica behind another was not refused")
	}
	for _, want := range []string{"site c holds WAL past site b", "c reaches 0/5A01C2F0", "b reaches 0/3000060", "--to c", "--accept-behind to tack plan and tack run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}

	req.AcceptBehind = true
	_, follow, err := Driver{}.Plan(w, topo, req)
	if err != nil {
		t.Fatalf("refused with --accept-behind: %v", err)
	}
	var sites []string
	for _, f := range follow {
		if f.Action == engine.FollowUpRebuild {
			sites = append(sites, f.Site)
		}
	}
	if strings.Join(sites, ",") != "a,c" {
		t.Errorf("Rebuild follow-ups for %v, want a and c", sites)
	}
	notes := strings.Join(Driver{}.Notes(w, topo, req), "\n")
	if !strings.Contains(notes, "warning: --accept-behind: site c reaches 0/5A01C2F0") {
		t.Errorf("notes %q do not warn of site c", notes)
	}

	// The most caught-up replica is not refused.
	ok := driver.Request{From: "a", To: "c", Mode: failoverv1.ModeSiteLoss}
	if _, follow, err := (Driver{}).Plan(w, topo, ok); err != nil || len(follow) != 1 {
		t.Errorf("site loss toward c: %v, follow-ups %v", err, follow)
	}
}

// Positions of another database, or that cannot be read, never refuse.
func TestPositionsThatDoNotCompare(t *testing.T) {
	c := threeSites(false)
	c.SetDown("a", true)
	other := replicaAt("0/5A01C2F0")
	other["systemID"] = "7300000000000000999"
	c.SetStatus("c", other, nil)
	w, topo := discover(t, c)
	req := driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss}
	if _, _, err := (Driver{}).Plan(w, topo, req); err != nil {
		t.Errorf("refused for another database: %v", err)
	}
	if s, _ := topo.Site("c"); !strings.HasPrefix(s.Position.Standing, "another database") {
		t.Errorf("site c: %q", s.Position.Standing)
	}

	c.SetStatus("c", replicaAt("0/5A01C2F0"), nil)
	c.SetStatus("b", nil, errors.New(`pods "pg-b-1" is forbidden: cannot get resource "pods/proxy"`))
	w, topo = discover(t, c)
	if _, _, err := (Driver{}).Plan(w, topo, req); err != nil {
		t.Errorf("refused when b's position cannot be read: %v", err)
	}
	notes := strings.Join(Driver{}.Notes(w, topo, req), "\n")
	for _, want := range []string{"warning: site b: the WAL position could not be read", "pods/proxy", "docs/operations.md#permissions"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes %q do not say %q", notes, want)
		}
	}
}

// RFC0004 Requirement 13: what a site-loss plan notes about the target.
func TestSiteLossNotes(t *testing.T) {
	objects := map[string][]client.Object{}
	for _, s := range []string{"a", "b", "c"} {
		var extra map[string]any
		if s == "b" {
			extra = map[string]any{"minApplyDelay": "1h"}
		}
		objects[s] = []client.Object{testenv.Cluster(s, "a", false, extra), testenv.Pooler(s, 0)}
	}
	c := testenv.NewClients(objects)
	c.SetDown("a", true)
	paused := replicaAt("0/3000060")
	paused["replayPaused"] = true
	c.SetStatus("b", paused, nil)
	w, topo := discover(t, c)
	notes := strings.Join(Driver{}.Notes(w, topo, driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss}), "\n")
	for _, want := range []string{
		"replication between sites is asynchronous. Site b reaches 0/3000060",
		"this run loses them (the RPO)",
		"warning: site b: Cluster sets spec.replica.minApplyDelay 1h",
		"warning: site b: WAL replay is paused",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes %q do not say %q", notes, want)
		}
	}
	planned := strings.Join(Driver{}.Notes(w, topo, driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}), "\n")
	if strings.Contains(planned, "RPO") || strings.Contains(planned, "minApplyDelay") {
		t.Errorf("a planned plan notes %q", planned)
	}
}

// #94, [RFC#0004] Requirement 13: with Tack's replay-time query, a
// site-loss plan says when the target's last replayed commit was. It
// reads the same /metrics GET, and keeps the replay lag.
func TestSiteLossSaysTheLastReplayedCommit(t *testing.T) {
	c := threeSites(false)
	c.SetDown("a", true)
	c.SetMetrics("b", "cnpg_pg_replication_lag 0\ncnpg_tack_replay_timestamp_seconds 1.7945604005e+09\n", nil)
	w, topo := discover(t, c)
	notes := strings.Join(Driver{}.Notes(w, topo, driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss}), "\n")
	want := "Site b reaches 0/3000060. Its last replayed commit is from 2026-11-13T09:00:00.5Z, by the old primary's clock"
	if !strings.Contains(notes, want) {
		t.Errorf("notes %q do not say %q", notes, want)
	}
}

// #94: the replay time, its age by Tack's clock, and what is printed
// without it.
func TestReplayTime(t *testing.T) {
	defer func(f func() time.Time) { now = f }(now)
	now = func() time.Time { return time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC) }
	c := threeSites(false)
	c.SetDown("a", true)
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	site := func(s string) *driver.Position {
		t.Helper()
		_, topo := discover(t, c)
		st, _ := topo.Site(s)
		return st.Position
	}
	notesTo := func(s string) string {
		w, topo := discover(t, c)
		return strings.Join(Driver{}.Notes(w, topo, driver.Request{From: "a", To: s, Mode: failoverv1.ModeSiteLoss, AcceptBehind: true}), "\n")
	}
	without := notesTo("b")

	c.SetMetrics("b", "cnpg_pg_replication_lag 0\ncnpg_tack_replay_timestamp_seconds 1794560400.5\n", nil)
	p := site("b")
	if p.LastCommit == nil || !p.LastCommit.Equal(at.Add(500*time.Millisecond)) || p.CommitAge != time.Hour-time.Second || p.CommitNote != "" || p.ReplayLag == nil {
		t.Errorf("site b: %+v", p)
	}
	if n := notesTo("b"); !strings.Contains(n, "Its last replayed commit is from 2026-11-13T09:00:00.5Z, by the old primary's clock (59m59s before this plan, by Tack's clock). The writes that the old primary committed after it are lost, and so may be a few committed just before it, under concurrent writes. The writes that the old primary took after that are not in it") || strings.Contains(n, "at least") {
		t.Errorf("notes %q", n)
	}

	// The old primary's clock ahead of Tack's.
	now = func() time.Time { return at.Add(-time.Minute) }
	if n := notesTo("b"); !strings.Contains(n, "by the old primary's clock (that clock is ahead of Tack's by 1m0s or more).") {
		t.Errorf("notes %q", n)
	}
	now = func() time.Time { return time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC) }

	// With disableDefaultQueries, the replay time alone.
	c.SetMetrics("b", "cnpg_tack_replay_timestamp_seconds 1794560400\n", nil)
	if p := site("b"); p.LastCommit == nil || p.ReplayLag != nil || !strings.Contains(p.LagError, "disableDefaultQueries") {
		t.Errorf("site b: %+v", p)
	}

	// NULL: no transaction replayed since the instance started.
	c.SetMetrics("b", "cnpg_pg_replication_lag 0\ncnpg_tack_replay_timestamp_seconds NaN\n", nil)
	if p := site("b"); p.LastCommit != nil || p.CommitNote != "replayed no transaction since it started" {
		t.Errorf("site b: %+v", p)
	}
	if n := notesTo("b"); !strings.Contains(n, "site b: replayed no transaction since it started, so the plan cannot say when its last replayed commit was") || strings.Contains(n, "last replayed commit is from") {
		t.Errorf("notes %q", n)
	}
	for _, bad := range []string{"0", "-1", "+Inf", "x"} {
		c.SetMetrics("b", "cnpg_pg_replication_lag 0\ncnpg_tack_replay_timestamp_seconds "+bad+"\n", nil)
		if p := site("b"); p.LastCommit != nil || !strings.Contains(p.CommitNote, "not a time") {
			t.Errorf("value %s: site b: %+v", bad, p)
		}
	}

	// Without the query, the notes are what they were.
	c.SetMetrics("b", "cnpg_pg_replication_lag 0\n", nil)
	if p := site("b"); p.LastCommit != nil || p.CommitNote != "" {
		t.Errorf("site b: %+v", p)
	}
	if n := notesTo("b"); n != without {
		t.Errorf("without the query, notes\n%s\nwant\n%s", n, without)
	}
}

// #94: the e2e's copy of the query is deploy/cnpg/replay-time.yaml, and
// the metric it makes is the one Tack reads: cnpg_<query>_<column>.
func TestReplayTimeQuery(t *testing.T) {
	read := func(path string) *corev1.ConfigMap {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cm := &corev1.ConfigMap{}
		if err := yaml.UnmarshalStrict(b, cm); err != nil {
			t.Fatal(err)
		}
		return cm
	}
	cm := read("../../../deploy/cnpg/replay-time.yaml")
	cp := read("../../../test/e2e/fixtures/gitops/clusters/a/replay-time.yaml")
	if cp.Namespace != "db" {
		t.Errorf("the e2e's copy is in namespace %q, want db", cp.Namespace)
	}
	cp.Namespace = ""
	if !reflect.DeepEqual(cm, cp) {
		t.Errorf("the e2e's copy differs from deploy/cnpg/replay-time.yaml:\n%+v\n%+v", cp, cm)
	}
	if _, ok := cm.Labels["cnpg.io/reload"]; !ok {
		t.Error("no cnpg.io/reload label: CloudNativePG would not reload the queries when they change")
	}
	var queries map[string]struct {
		Query   string                         `json:"query"`
		Metrics []map[string]map[string]string `json:"metrics"`
	}
	if err := yaml.UnmarshalStrict([]byte(cm.Data["queries"]), &queries); err != nil {
		t.Fatal(err)
	}
	var names []string
	for q, v := range queries {
		if !strings.Contains(v.Query, "pg_last_xact_replay_timestamp()") {
			t.Errorf("query %s does not read pg_last_xact_replay_timestamp()", q)
		}
		for _, m := range v.Metrics {
			for col, spec := range m {
				names = append(names, "cnpg_"+q+"_"+col+" "+spec["usage"])
			}
		}
	}
	if strings.Join(names, ",") != ReplayTimeMetric+" GAUGE" {
		t.Errorf("the query makes %v, want %s GAUGE", names, ReplayTimeMetric)
	}
}
