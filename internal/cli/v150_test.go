package cli

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/testenv"
	"github.com/takodotid/tack/internal/version"
)

// v150Ledger reads a ledger that tack v1.5.0 wrote, created at the time of
// its name. testdata/ledgers/v1.5.0 holds the ledgers that v1.5.0's own
// code left on the fakes of its CLI tests, with its version set to v1.5.0,
// its clock at each step's time, and each ledger's creation stamped with
// it, as an API server does:
//
//	planned                 09:00, a to b, Succeeded; ledger at b
//	site-loss-rebuild       10:00, b lost, to a, Succeeded once resumed
//	                        with --fence b at 10:05; follow-up Rebuild b;
//	                        ledger at a
//	planned-after-rebuild   11:00, a to b, Succeeded, after b's member was
//	                        created again at 10:30; ledger at b
//	abandoned               12:00, b to a, stopped at verify-git-a, then
//	                        abandoned at 12:10; ledger at a
func v150Ledger(t *testing.T, file string) *failoverv1.SiteFailover {
	t.Helper()
	b, err := os.ReadFile("testdata/ledgers/v1.5.0/" + file + ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	var sf failoverv1.SiteFailover
	if err := yaml.UnmarshalStrict(b, &sf); err != nil {
		t.Fatal(err)
	}
	created, err := time.Parse("20060102-150405", strings.TrimPrefix(sf.Name, "pg-main-"))
	if err != nil {
		t.Fatal(err)
	}
	sf.CreationTimestamp = metav1.NewTime(created)
	return &sf
}

// #58, #66. Ledgers that v1.5.0 wrote stay usable under this tack: tack
// status lists them, and tack prune decides on them by the rules of
// [RFC#0003] Requirement 27. Under v1.5.0, prune kept the site-loss run for
// its follow-up Rebuild whatever b's member was; it now deletes it once
// that member was created again 10 s or more after the run started, and
// keeps it otherwise.
func TestLedgersOfV150(t *testing.T) {
	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "v1.5.0-1-g0000000"
	files := []string{"planned", "site-loss-rebuild", "planned-after-rebuild", "abandoned"}
	names := map[string]string{}
	for _, file := range files {
		sf := v150Ledger(t, file)
		if sf.Status.WrittenBy != "v1.5.0" {
			t.Fatalf("%s: writtenBy %q, want v1.5.0", file, sf.Status.WrittenBy)
		}
		names[file] = sf.Name
	}
	for _, tt := range []struct {
		name string
		// rebuilt is when b's member was created.
		rebuilt time.Time
		deleted []string
		kept    map[string]string
	}{
		{
			name:    "b's member created again at 10:30",
			rebuilt: time.Date(2026, 11, 12, 10, 30, 0, 0, time.UTC),
			deleted: []string{"planned", "site-loss-rebuild"},
			kept:    map[string]string{"planned-after-rebuild": "keep: one of the 1 newest that succeeded", "abandoned": "keep: abandoned"},
		},
		{
			name:    "b's member not created again",
			rebuilt: time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC),
			deleted: []string{"planned"},
			kept: map[string]string{
				"planned-after-rebuild": "keep: one of the 1 newest that succeeded",
				"abandoned":             "keep: abandoned",
				"site-loss-rebuild":     "keep: follow-up Rebuild for site b: its member was created at 2026-11-12T08:00:00Z, not 10s or more after the run's ledger at 2026-11-12T10:00:00Z",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			at := map[string]string{"planned": "b", "site-loss-rebuild": "a", "planned-after-rebuild": "b", "abandoned": "a"}
			objects := map[string][]client.Object{
				"a": testenv.Site("a", "a", 2),
				"b": {created(testenv.Cluster("b", "a", false, nil), tt.rebuilt), testenv.Pooler("b", 0)},
			}
			for _, file := range files {
				objects[at[file]] = append(objects[at[file]], v150Ledger(t, file))
			}
			f := newFactory(objects)
			out, code := run(t, f, "status", "-f", "testdata/workload.yaml")
			if code != ExitOK {
				t.Fatalf("status: exit %d:\n%s", code, out)
			}
			for file, state := range map[string]string{"planned": "Succeeded", "site-loss-rebuild": "Succeeded", "planned-after-rebuild": "Succeeded", "abandoned": "Abandoned"} {
				if !slices.ContainsFunc(strings.Split(out, "\n"), func(l string) bool {
					f := strings.Fields(l)
					return len(f) > 5 && f[1] == names[file] && f[5] == state
				}) {
					t.Errorf("status lists %s not as %s:\n%s", names[file], state, out)
				}
			}
			out, code = run(t, f, "prune", "-f", "testdata/workload.yaml", "--keep", "1", "--yes")
			if code != ExitOK {
				t.Fatalf("prune: exit %d:\n%s", code, out)
			}
			for _, file := range tt.deleted {
				if !strings.Contains(out, names[file]+"  ") || slices.Contains(runsAt(t, f, at[file]), names[file]) {
					t.Errorf("%s was not deleted:\n%s", file, out)
				}
			}
			for file, why := range tt.kept {
				if !slices.ContainsFunc(strings.Split(out, "\n"), func(l string) bool {
					return strings.Contains(l, names[file]+" ") && strings.HasSuffix(l, why)
				}) || !slices.Contains(runsAt(t, f, at[file]), names[file]) {
					t.Errorf("%s was not kept with %q:\n%s", file, why, out)
				}
			}
		})
	}
}
