package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/config"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/driver/cnpg"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/testenv"
)

// siteNames returns the names of n sites: a, b, c and on.
func siteNames(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, string(rune('a'+i)))
	}
	return out
}

// workloadN is a Workload of the sites, each with a Cluster and a Pooler.
func workloadN(tb testing.TB, sites []string) *config.Workload {
	tb.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: failover.tako.id/v1\nkind: Workload\nmetadata: {name: pg-main}\nspec:\n  driver: cnpg\n  sites:\n")
	for _, s := range sites {
		fmt.Fprintf(&b, "  - {name: %s, kubeContext: site-%s, cnpg: {cluster: {namespace: db, name: pg-%s}, pooler: {namespace: db, name: pg-%s-pooler-rw, instances: 2}}}\n", s, s, s, s)
	}
	b.WriteString("  git: {url: https://git.example.com/gitops.git, branch: main, find: {}}\n  timeouts: {demotion: 5s, promotion: 5s, flux: 5s}\n")
	w, err := config.Parse([]byte(b.String()))
	if err != nil {
		tb.Fatal(err)
	}
	return w
}

// worstLedger runs a planned switchover from a to b on n sites to its end,
// and returns its ledger then, and with its history filled to the bound:
// 100 entries, each with a message of the most bytes the history keeps.
// Step messages have no bound: they are as the run wrote them.
func worstLedger(tb testing.TB, n int) (ran, worst *failoverv1.SiteFailover) {
	tb.Helper()
	sites := siteNames(n)
	w := workloadN(tb, sites)
	objs := map[string][]client.Object{}
	for _, s := range sites {
		pool := int64(0)
		if s == "a" {
			pool = 2
		}
		objs[s] = testenv.Site(s, "a", pool)
	}
	clients := testenv.NewClients(objs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&testenv.Operator{Clients: clients, Sites: sites}).Run(ctx)
	store := &ledger.Store{Client: clients.Sites["b"], Namespace: "tack-system"}
	env := &engine.Env{
		Clients: clients, Driver: cnpg.Driver{}, Flux: testenv.NewFlux("main@sha1:1111111111111111111111111111111111111111"), Git: &testenv.Git{},
		Ledger: store, Out: io.Discard, Poll: time.Millisecond,
		ReachWaits: []time.Duration{time.Millisecond, time.Millisecond},
	}
	p, err := env.MakePlan(ctx, engine.Request{Run: "pg-main-20261027-101500", Workload: w.Name, Spec: &w.Spec,
		Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModePlanned}})
	if err != nil {
		tb.Fatal(err)
	}
	if err := env.Start(ctx, p.SiteFailover, engine.Options{}); err != nil {
		tb.Fatal(err)
	}
	sf, err := store.Get(ctx, p.SiteFailover.Name)
	if err != nil {
		tb.Fatal(err)
	}
	ran = sf.DeepCopy()
	msg := strings.Repeat("x", failoverv1.MaxHistoryMessage)
	for len(sf.Status.History) < failoverv1.MaxHistory {
		engine.AppendHistory(&sf.Status, failoverv1.HistoryEntry{
			Time: metav1.Now(), State: failoverv1.HistoryState(failoverv1.RunStopped), Reason: string(failoverv1.ReasonStepFailed),
			Message: msg, Version: "v1.3.0", User: "system:serviceaccount:tack-system:operator", Flags: []string{"--yes"},
		})
	}
	return ran, sf
}

// BenchmarkLedgerBytes measures the JSON size of the ledger of a run on 2
// and on 10 sites (#35 row 47), the size of each item of a list of
// ledgers: run-bytes as the run left it, bytes with a full history. Run it
// with -benchtime=1x.
func BenchmarkLedgerBytes(b *testing.B) {
	for _, n := range []int{2, 10} {
		b.Run(fmt.Sprintf("sites=%d", n), func(b *testing.B) {
			ran, sf := worstLedger(b, n)
			run, _ := json.Marshal(ran)
			spec, _ := json.Marshal(sf.Spec)
			status, _ := json.Marshal(sf.Status)
			all, _ := json.Marshal(sf)
			for b.Loop() {
			}
			b.ReportMetric(float64(len(sf.Spec.Steps)), "steps")
			b.ReportMetric(float64(len(spec)), "spec-bytes")
			b.ReportMetric(float64(len(status)), "status-bytes")
			b.ReportMetric(float64(len(run)), "run-bytes")
			b.ReportMetric(float64(len(all)), "bytes")
		})
	}
}

// runsN returns n finished runs of a workload, as a search finds them.
func runsN(n int) []failoverv1.SiteFailover {
	runs := make([]failoverv1.SiteFailover, n)
	start := time.Date(2026, 10, 27, 10, 15, 0, 0, time.UTC)
	for i := range runs {
		r := &runs[i]
		r.Name = fmt.Sprintf("pg-main-%06d", i)
		r.CreationTimestamp = metav1.NewTime(start.Add(time.Duration(i) * time.Minute))
		r.Spec.To = "b"
		r.Status.State = failoverv1.RunSucceeded
	}
	return runs
}

// BenchmarkActiveRunSearch measures what the search for active runs does
// with the runs it found, as checkStarted and tack run's check do: Ended
// for each run (#35 row 47).
func BenchmarkActiveRunSearch(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("runs=%d", n), func(b *testing.B) {
			runs := runsN(n)
			// One run that is not finished, the worst case: Ended reads
			// every other run's spec for it.
			runs[n/2].Status.State = failoverv1.RunStopped
			for b.Loop() {
				active := 0
				for i := range runs {
					if !engine.Ended(&runs[i], runs) {
						active++
					}
				}
				if active != 1 {
					b.Fatalf("%d active runs", active)
				}
			}
		})
	}
}
