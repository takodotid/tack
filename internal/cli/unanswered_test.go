package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/testenv"
)

// runSplit runs tack as run does, with stdout and stderr apart.
func runSplit(t *testing.T, f Factory, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	nowFunc = func() time.Time { return fixedNow }
	reachWaits = testWaits
	var out, errs bytes.Buffer
	code = Execute(context.Background(), f, args, &out, &errs)
	return out.String(), errs.String(), code
}

// yamlDocs splits a YAML stream into its documents, as kubectl reads it.
func yamlDocs(t *testing.T, s string) []failoverv1.SiteFailover {
	t.Helper()
	var out []failoverv1.SiteFailover
	for _, doc := range strings.Split(s, "\n---\n") {
		var sf failoverv1.SiteFailover
		if err := yaml.UnmarshalStrict([]byte(doc), &sf); err != nil {
			t.Fatalf("the output does not parse as YAML: %v\n%s", err, s)
		}
		if sf.Name != "" {
			out = append(out, sf)
		}
	}
	return out
}

// #59, observed in a two-site test of v1.3.0: with one site unreachable
// after its 3 checks, tack status exited 0 and left out that site's runs,
// with no line naming the site; with both sites down, it printed an empty
// table and exited 0. It lists what it read, names each site that gave no
// answer with its error, and exits 1. -o yaml names them as comments after
// the ledgers, and still parses.
func TestStatusNamesSitesThatGaveNoAnswer(t *testing.T) {
	const noAnswerA = "site a: no answer to 3 checks over 0s: site a is unreachable: connection refused"
	const noAnswerB = "site b: no answer to 3 checks over 0s: site b is unreachable: connection refused"
	tests := []struct {
		name    string
		down    []string
		output  string
		code    int
		runs    []string
		missing []string
		err     string
	}{
		{name: "every site answers", runs: []string{"pg-main-at-a", "pg-main-at-b"}},
		{name: "site a down", down: []string{"a"}, code: ExitError, runs: []string{"pg-main-at-b"}, missing: []string{noAnswerA},
			err: "tack: the list of runs is incomplete: site a gave no answer, so the runs there are not listed\n"},
		{name: "both sites down", down: []string{"a", "b"}, code: ExitError, missing: []string{noAnswerA, noAnswerB},
			err: "tack: the list of runs is incomplete: sites a, b gave no answer, so the runs there are not listed\n"},
		{name: "site a down, yaml", down: []string{"a"}, output: "yaml", code: ExitError, runs: []string{"pg-main-at-b"}, missing: []string{noAnswerA},
			err: "tack: the list of runs is incomplete: site a gave no answer, so the runs there are not listed\n"},
		{name: "both sites down, yaml", down: []string{"a", "b"}, output: "yaml", code: ExitError, missing: []string{noAnswerA, noAnswerB},
			err: "tack: the list of runs is incomplete: sites a, b gave no answer, so the runs there are not listed\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atA, atB := stoppedRun(t, "pg-main-at-a"), stoppedRun(t, "pg-main-at-b")
			f := newFactory(map[string][]client.Object{
				"a": append(testenv.Site("a", "a", 2), atA),
				"b": append(testenv.Site("b", "a", 0), atB),
			}, tt.down...)
			args := []string{"status", "-f", "testdata/workload.yaml"}
			if tt.output != "" {
				args = append(args, "-o", tt.output)
			}
			out, errs, code := runSplit(t, f, args...)
			if code != tt.code {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.code, out, errs)
			}
			// The error line is the last line of stderr; the lines before
			// it are the checks' retries.
			if tt.err != "" && !strings.HasSuffix(errs, tt.err) {
				t.Errorf("stderr does not end with %q:\n%s", tt.err, errs)
			}
			if tt.err == "" && strings.Contains(errs, "tack: ") {
				t.Errorf("stderr has an error:\n%s", errs)
			}
			var listed []string
			if tt.output == "yaml" {
				for _, sf := range yamlDocs(t, out) {
					listed = append(listed, sf.Name)
				}
			} else {
				for _, line := range strings.Split(out, "\n") {
					if f := strings.Fields(line); len(f) > 1 && strings.HasPrefix(f[1], "pg-main-") {
						listed = append(listed, f[1])
					}
				}
			}
			if strings.Join(listed, " ") != strings.Join(tt.runs, " ") {
				t.Errorf("listed %v, want %v:\n%s", listed, tt.runs, out)
			}
			var want string
			if len(tt.missing) > 0 {
				if tt.output == "yaml" {
					want = "# Sites that gave no answer; their runs are not listed:\n#   " + strings.Join(tt.missing, "\n#   ") + "\n"
				} else {
					want = "\nSites that gave no answer; their runs are not listed:\n  " + strings.Join(tt.missing, "\n  ") + "\n"
				}
			}
			if !strings.HasSuffix(out, want) || want == "" && strings.Contains(out, "gave no answer") {
				t.Errorf("stdout does not end with %q:\n%s", want, out)
			}
		})
	}
}

// #59: tack status RUN, tack resume RUN and tack abandon RUN said only
// "not found at any reachable site" for a run whose site gave no answer.
// The error names each site that gave no answer, with its error.
func TestNamedRunNotFoundNamesSites(t *testing.T) {
	const want = `run "pg-main-at-a" not found at any reachable site; site a gave no answer, so its runs were not read: no answer to 3 checks over 0s: site a is unreachable: connection refused`
	for _, cmd := range [][]string{
		{"status", "pg-main-at-a"},
		{"resume", "pg-main-at-a", "--yes"},
		{"abandon", "pg-main-at-a", "--yes"},
	} {
		t.Run(cmd[0], func(t *testing.T) {
			f := newFactory(map[string][]client.Object{
				"a": append(testenv.Site("a", "a", 2), stoppedRun(t, "pg-main-at-a")),
				"b": testenv.Site("b", "a", 0),
			}, "a")
			out, code := run(t, f, append(cmd, "-f", "testdata/workload.yaml")...)
			if code != ExitError || !strings.Contains(out, want) {
				t.Fatalf("exit %d, want %d with %q:\n%s", code, ExitError, want, out)
			}
		})
	}
}

// #59: the searches of tack plan, tack run and tack resume went on
// without a site that gave no answer, as RFC0003 Requirements 8 and 18
// say, with no line after its retries; tack plan -o yaml prints no note.
// Each names the site on a progress line, once per process.
func TestSearchesNameSitesLeftOut(t *testing.T) {
	const lineA = "  not read  the runs at site a: "
	t.Run("plan -o yaml", func(t *testing.T) {
		f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)}, "a")
		out, errs, code := runSplit(t, f, "plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss", "-o", "yaml")
		if code != ExitOK {
			t.Fatalf("exit %d:\n%s%s", code, out, errs)
		}
		if n := strings.Count(errs, lineA+"site a is unreachable: connection refused\n"); n != 1 {
			t.Errorf("stderr names site a %d times, want once:\n%s", n, errs)
		}
	})
	t.Run("run", func(t *testing.T) {
		// Site c answers the plan, then not the search before the run
		// starts. The search after the ledger's creation reuses that
		// search's checks.
		f := newFactory(map[string][]client.Object{
			"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0), "c": testenv.Site("c", "a", 0),
		})
		f.clients.Flap("c", true, false, false, false)
		f.flux.Fail["Reconcile"] = errors.New("stop after the holds")
		out, code := run(t, f, "run", "-f", "testdata/workload-3.yaml", "--to", "b", "--yes")
		if code != ExitStopped {
			t.Fatalf("exit %d, want %d at the first reconcile:\n%s", code, ExitStopped, out)
		}
		const lineC = "  not read  the runs at site c: no answer to 3 checks over 0s: site c is unreachable: context deadline exceeded\n"
		if n := strings.Count(out, lineC); n != 1 {
			t.Errorf("the output names site c %d times, want once:\n%s", n, out)
		}
	})
	t.Run("resume", func(t *testing.T) {
		// The run is found at b. The search for a newer run, before the
		// confirmation, cannot read site a.
		f := newFactory(map[string][]client.Object{
			"a": testenv.Site("a", "b", 0),
			"b": append(testenv.Site("b", "b", 2), stoppedRun(t, "pg-main-older")),
		}, "a")
		out, code := run(t, f, "resume", "pg-main-older", "-f", "testdata/workload.yaml")
		if code != ExitUsage || !strings.Contains(out, "no terminal to confirm on") {
			t.Fatalf("exit %d, want %d at the confirmation:\n%s", code, ExitUsage, out)
		}
		if n := strings.Count(out, lineA+"no answer to 3 checks over 0s: site a is unreachable: connection refused\n"); n != 1 {
			t.Errorf("the output names site a %d times, want once:\n%s", n, out)
		}
	})
}
