package cli

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/driver"
	_ "github.com/takodotid/tack/internal/driver/cnpg"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/testenv"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var fixedNow = time.Date(2026, 10, 27, 10, 15, 0, 0, time.UTC)

type testFactory struct {
	clients *testenv.Clients
	flux    *testenv.Flux
	git     *testenv.Git
}

func (f *testFactory) Clients(string, map[string]string) kube.Clients { return f.clients }

func (f *testFactory) Env(_ context.Context, _ string, spec *v1alpha1.WorkloadSpec, ledgerSite string, out io.Writer) (*engine.Env, func(), error) {
	d, err := driver.Get(spec.Driver)
	if err != nil {
		return nil, nil, err
	}
	env := &engine.Env{
		Clients: f.clients, Driver: d, Flux: f.flux, Git: f.git, Out: out,
		Now: func() time.Time { return fixedNow }, Poll: time.Millisecond,
	}
	if cl, err := f.clients.Client(ledgerSite); err == nil {
		env.Ledger = &ledger.Store{Client: cl, Namespace: spec.Ledger.Namespace}
	}
	return env, func() {}, nil
}

func newFactory(objects map[string][]client.Object, down ...string) *testFactory {
	f := &testFactory{clients: testenv.NewClients(objects), flux: testenv.NewFlux("main@sha1:1111111111111111111111111111111111111111"), git: &testenv.Git{}}
	for _, s := range down {
		f.clients.SetDown(s, true)
	}
	return f
}

func run(t *testing.T, f Factory, args ...string) (string, int) {
	t.Helper()
	nowFunc = func() time.Time { return fixedNow }
	var out bytes.Buffer
	code := Execute(context.Background(), f, args, &out, &out)
	return out.String(), code
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/cli -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file; run go test ./internal/cli -update and review the diff\n--- got ---\n%s", path, got)
	}
}

func TestPlanGolden(t *testing.T) {
	tests := []struct {
		name    string
		objects map[string][]client.Object
		down    []string
		gitDown bool
		args    []string
		code    int
	}{
		{
			name:    "plan-planned",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b"},
		},
		{
			name:    "plan-site-loss",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			down:    []string{"a"},
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss"},
		},
		{
			name:    "plan-site-loss-git-down",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			down:    []string{"a"},
			gitDown: true,
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss"},
		},
		{
			name: "plan-three-sites-strait",
			objects: map[string][]client.Object{
				"a": {testenv.Cluster("a", "a", true, nil)},
				"b": {testenv.Cluster("b", "a", false, nil)},
				"c": {testenv.Cluster("c", "a", false, nil)},
			},
			args: []string{"plan", "-f", "testdata/workload-3.yaml", "--to", "b"},
		},
		{
			name:    "plan-planned-from-down",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			down:    []string{"a"},
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b"},
			code:    ExitError,
		},
		{
			name:    "plan-site-loss-from-primary",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss"},
			code:    ExitError,
		},
		{
			name: "plan-planned-diverged",
			objects: map[string][]client.Object{
				"a": {promotedTwice(testenv.Cluster("a", "a", true, nil)), testenv.Pooler("a", 2)},
				"b": {testenv.Cluster("b", "a", false, nil), testenv.Pooler("b", 0)},
			},
			args: []string{"plan", "-f", "testdata/workload.yaml", "--to", "b"},
			code: ExitError,
		},
		{
			name:    "plan-bad-mode",
			objects: map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)},
			args:    []string{"plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "fast"},
			code:    ExitUsage,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFactory(tt.objects, tt.down...)
			f.git.Unreachable = tt.gitDown
			out, code := run(t, f, tt.args...)
			if code != tt.code {
				t.Fatalf("exit code %d, want %d\n%s", code, tt.code, out)
			}
			golden(t, tt.name, out)
			if len(f.flux.Calls) > 0 || len(f.git.Commits) > 0 || f.clients.Writes > 0 {
				t.Errorf("plan changed something: Flux %v, Git %d commits, %d cluster writes", f.flux.Calls, len(f.git.Commits), f.clients.Writes)
			}
		})
	}
}

// promotedTwice puts the primary on timeline 2, as after a failover that
// left the other site's member behind on timeline 1.
func promotedTwice(u *unstructured.Unstructured) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(u.Object, int64(2), "status", "instancesReportedState", "pg-a-1", "timeLineID")
	return u
}

func TestSkill(t *testing.T) {
	out, code := run(t, newFactory(nil), "--skill")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.HasPrefix(out, "---\nname: tack\ndescription: ") {
		t.Fatalf("no skill frontmatter:\n%s", out)
	}
	for _, want := range []string{
		`An agent never starts "tack run" or "tack resume" without an explicit`,
		`"tack plan" comes first`,
		"### tack plan", "### tack run", "### tack resume", "### tack status",
		"`--accept-git-head`", "`--fence`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("skill lacks %q", want)
		}
	}
	golden(t, "skill", out)
}

func TestRunNeedsConfirmation(t *testing.T) {
	isTerminal = func() bool { return false }
	defer func() { isTerminal = stdinIsTerminal }()
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b")
	if code != ExitUsage || !strings.Contains(out, "pass --yes once a human approved this run") {
		t.Fatalf("exit %d, want %d with a refusal:\n%s", code, ExitUsage, out)
	}
	if len(f.flux.Held) != 0 {
		t.Fatal("run held objects without confirmation")
	}
}

func TestRunWrongConfirmation(t *testing.T) {
	isTerminal = func() bool { return true }
	stdin = strings.NewReader("yes\n")
	defer func() { isTerminal, stdin = stdinIsTerminal, os.Stdin }()
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	out, code := run(t, f, "run", "-f", "testdata/workload.yaml", "--to", "b")
	if code != ExitError || !strings.Contains(out, "not confirmed; nothing was changed") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}
