package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
)

// noCluster is a Factory that fails the test when a command asks it for
// anything: completion contacts no cluster.
type noCluster struct{ t *testing.T }

func (n noCluster) Clients(string, map[string]string) kube.Clients {
	n.t.Fatal("completion asked for the clients of a cluster")
	return nil
}

func (n noCluster) Env(context.Context, string, *failoverv1.WorkloadSpec, string, io.Writer) (*engine.Env, func(), error) {
	n.t.Fatal("completion asked for an Env")
	return nil, nil, nil
}

// #79, [RFC#0009] Requirement 33: completion of commands, flag values,
// Workload files and the file's site names, without a cluster.
func TestCompletion(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want []string
		not  []string
	}{
		{[]string{""}, []string{"plan", "run", "resume", "status", "abandon", "prune", "completion"}, nil},
		{[]string{"plan", "--mode", ""}, []string{"planned", "site-loss", ":4"}, nil},
		{[]string{"status", "-o", ""}, []string{"text", "yaml", "json", ":4"}, nil},
		{[]string{"plan", "-f", ""}, []string{"yaml", "yml", ":8"}, nil},
		{[]string{"plan", "-f", "testdata/workload-3.yaml", "--to", ""}, []string{"a\tcontext site-a", "b\tcontext site-b", "c\tcontext site-c", ":4"}, nil},
		{[]string{"resume", "RUN", "-f", "testdata/workload.yaml", "--fence", ""}, []string{"a\tcontext site-a", ":4"}, nil},
		{[]string{"plan", "--to", ""}, []string{":4"}, []string{"site-a"}},
		{[]string{"resume", ""}, []string{":4"}, []string{"pg-main"}},
		{[]string{"completion", ""}, []string{"bash", "zsh", "fish", "powershell"}, nil},
	} {
		var out, errw bytes.Buffer
		code := runOn(t, noCluster{t}, &out, &errw, append([]string{"__complete"}, tt.args...)...)
		if code != ExitOK {
			t.Errorf("%v: exit %d: %s", tt.args, code, errw.String())
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		for _, w := range tt.want {
			found := false
			for _, l := range lines {
				found = found || l == w || strings.HasPrefix(l, w+"\t")
			}
			if !found {
				t.Errorf("%v: completes %q, want %q", tt.args, lines, w)
			}
		}
		for _, n := range tt.not {
			if strings.Contains(out.String(), n) {
				t.Errorf("%v: completes %q", tt.args, out.String())
			}
		}
	}
}

// tack completion prints a script for each shell, and refuses another.
func TestCompletionScripts(t *testing.T) {
	for shell, want := range map[string]string{"bash": "__start_tack", "zsh": "#compdef tack", "fish": "complete -c tack", "powershell": "Register-ArgumentCompleter"} {
		var out, errw bytes.Buffer
		if code := runOn(t, noCluster{t}, &out, &errw, "completion", shell); code != ExitOK || !strings.Contains(out.String(), want) {
			t.Errorf("%s: exit %d, lacks %q", shell, code, want)
		}
	}
	var out, errw bytes.Buffer
	if code := runOn(t, noCluster{t}, &out, &errw, "completion", "tcsh"); code != ExitUsage {
		t.Errorf("tcsh: exit %d, want %d", code, ExitUsage)
	}
}
