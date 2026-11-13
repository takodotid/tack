package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/testenv"
)

// behindSites are three sites after the loss of a: b is behind c.
func behindSites() *testFactory {
	objects := map[string][]client.Object{}
	for _, s := range []string{"a", "b", "c"} {
		objects[s] = []client.Object{testenv.Cluster(s, "a", s == "a", nil)}
	}
	// c does not name pg-b among its external clusters: a run stops at
	// declare-c, after the promotion ([RFC#0004] Requirement 12).
	c := objects["c"][0].(*unstructured.Unstructured)
	c.Object["spec"].(map[string]any)["externalClusters"] = []any{map[string]any{"name": "pg-a"}, map[string]any{"name": "pg-c"}}
	f := newFactory(objects, "a")
	at := func(lsn string) map[string]any {
		return map[string]any{"systemID": "7300000000000000001", "timeLineID": 1, "receivedLsn": lsn, "replayLsn": lsn}
	}
	f.clients.SetStatus("b", at("0/3000060"), nil)
	f.clients.SetStatus("c", at("0/5A01C2F0"), nil)
	return f
}

// RFC0004 Requirement 14: a site loss toward a replica behind another is
// refused before its ledger exists, and the refusal names the override.
// With --accept-behind the ledger records it, its history names the flag,
// the run records a Rebuild for the site ahead, and a resume before the
// Failover phase reads the sites again and goes on.
func TestAcceptBehindIsRecorded(t *testing.T) {
	f := behindSites()
	out, code := run(t, f, "run", "-f", "testdata/workload-3.yaml", "--to", "b", "--mode", "site-loss", "--yes")
	if code != ExitError || !strings.Contains(out, "site c holds WAL past site b") || !strings.Contains(out, "--accept-behind") {
		t.Fatalf("exit %d, want %d with the refusal:\n%s", code, ExitError, out)
	}
	store := &ledger.Store{Client: f.clients.Sites["b"], Namespace: "tack-system"}
	ctx := context.Background()
	if runs, err := store.List(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("the refused run left ledgers %v, %v", runs, err)
	}

	f.flux.Fail["Hold"] = errors.New("stop at the first hold")
	out, code = run(t, f, "run", "-f", "testdata/workload-3.yaml", "--to", "b", "--mode", "site-loss", "--accept-behind", "--yes")
	if code != ExitStopped || !strings.Contains(out, "warning: --accept-behind: site c reaches 0/5A01C2F0") {
		t.Fatalf("exit %d, want %d with the warning:\n%s", code, ExitStopped, out)
	}
	sf, err := store.Get(ctx, "pg-main-20261027-101500")
	if err != nil || !sf.Spec.AcceptBehind {
		t.Fatalf("ledger: %v, spec.acceptBehind %v", err, sf != nil && sf.Spec.AcceptBehind)
	}
	if h := sf.Status.History; len(h) == 0 || !slices.Contains(h[0].Flags, "--accept-behind") {
		t.Errorf("history %+v does not record --accept-behind", h)
	}
	var rebuild []string
	for _, fu := range sf.Status.FollowUps {
		rebuild = append(rebuild, fu.Site+" "+fu.Action)
	}
	if !slices.Equal(rebuild, []string{"a Rebuild", "c Rebuild"}) {
		t.Errorf("follow-ups %v, want a Rebuild for a and for c", rebuild)
	}

	// The resume reads the sites again before the Failover phase, and goes
	// on: it promotes b. c, which does not name pg-b among its external
	// clusters, then stops it at declare-c, so the test ends there.
	delete(f.flux.Fail, "Hold")
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go (&testenv.Operator{Clients: f.clients, Sites: []string{"a", "b", "c"}}).Run(ctx)
	out, code = run(t, f, "resume", "pg-main-20261027-101500", "-f", "testdata/workload-3.yaml", "--yes")
	if code != ExitStopped || strings.Contains(out, "holds WAL past") || !strings.Contains(out, "declare-c") {
		t.Fatalf("resume: exit %d, want %d at declare-c:\n%s", code, ExitStopped, out)
	}
	sf, err = store.Get(ctx, "pg-main-20261027-101500")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range sf.Status.Steps {
		if st.ID == "promote-b" && st.State != "Succeeded" {
			t.Errorf("promote-b: %s, want Succeeded", st.State)
		}
	}
}

// --accept-behind is for a site loss: a planned switchover loses no write.
func TestAcceptBehindNeedsSiteLoss(t *testing.T) {
	f := behindSites()
	for _, args := range [][]string{{"plan"}, {"run", "--yes"}} {
		cmd := args[0]
		out, code := run(t, f, append(args, "-f", "testdata/workload-3.yaml", "--to", "b", "--accept-behind")...)
		if code != ExitUsage || !strings.Contains(out, "--accept-behind is for --mode site-loss") {
			t.Errorf("%s: exit %d, want %d:\n%s", cmd, code, ExitUsage, out)
		}
	}
}

// A run that stopped before its Failover steps, whose target is then
// behind another replica, is refused on resume by the read before the
// first Failover step. tack resume has no --accept-behind: the message
// names the commands that take it, and the way on.
func TestResumeRefusesTargetNowBehind(t *testing.T) {
	f := behindSites()
	f.clients.SetStatus("c", map[string]any{"systemID": "7300000000000000001", "timeLineID": 1, "receivedLsn": "0/3000060", "replayLsn": "0/3000060"}, nil)
	f.flux.Fail["Hold"] = errors.New("stop at the first hold")
	if out, code := run(t, f, "run", "-f", "testdata/workload-3.yaml", "--to", "b", "--mode", "site-loss", "--yes"); code != ExitStopped {
		t.Fatalf("run: exit %d:\n%s", code, out)
	}
	delete(f.flux.Fail, "Hold")
	f.clients.SetStatus("c", map[string]any{"systemID": "7300000000000000001", "timeLineID": 1, "receivedLsn": "0/5A01C2F0", "replayLsn": "0/5A01C2F0"}, nil)
	out, code := run(t, f, "resume", "pg-main-20261027-101500", "-f", "testdata/workload-3.yaml", "--yes")
	for _, want := range []string{"site c holds WAL past site b", "pass --accept-behind to tack plan and tack run", "No Failover step of this run ran", "abandon the run"} {
		if !strings.Contains(out, want) {
			t.Errorf("resume (exit %d) does not say %q:\n%s", code, want, out)
		}
	}
	if code != ExitStopped {
		t.Errorf("resume: exit %d, want %d", code, ExitStopped)
	}
}
