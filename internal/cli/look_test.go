package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/internal/testenv"
)

// screen is a terminal of a given width, for tests.
type screen struct {
	bytes.Buffer
	width int
}

func (*screen) IsTerminal() bool { return true }
func (s *screen) Width() int     { return s.width }

func runOn(t *testing.T, f Factory, out, errw io.Writer, args ...string) int {
	t.Helper()
	nowFunc = func() time.Time { return fixedNow }
	reachWaits = testWaits
	return Execute(context.Background(), f, args, out, errw)
}

// siteLossFactory is two sites after the loss of a.
func siteLossFactory() *testFactory {
	return newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)}, "a")
}

// #77, [RFC#0009] Requirement 31: on a terminal, the plan has bold headings,
// tables with lines, the most caught-up site in green, and its notes
// heading in yellow; progress lines have their time dim and their word
// coloured.
func TestPlanOnATerminal(t *testing.T) {
	out, errw := &screen{width: 120}, &screen{width: 120}
	if code := runOn(t, siteLossFactory(), out, errw, "plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss"); code != ExitOK {
		t.Fatalf("exit %d:\n%s%s", code, out, errw)
	}
	for _, want := range []string{
		ansiBold + "Sites:" + ansiReset,
		"┌", " " + ansiBold + "Site" + ansiReset + " ", "└",
		ansiGreen + "most caught up" + ansiReset,
		ansiBold + ansiYellow + "Notes:" + ansiReset,
		ansiYellow + "Rebuild" + ansiReset,
		ansiDim + "patch Cluster db/pg-b (merge)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan on a terminal lacks %q:\n%s", want, out)
		}
	}
	for _, want := range []string{ansiDim + "2026-10-27T10:15:00Z " + ansiReset, ansiYellow + "not read" + ansiReset} {
		if !strings.Contains(errw.String(), want) {
			t.Errorf("progress lines lack %q:\n%q", want, errw)
		}
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if visible(line) > 120 && strings.HasPrefix(line, "  │") {
			t.Errorf("a table line wider than the terminal: %d", visible(line))
		}
	}
}

// NO_COLOR keeps the symbols and tables, without any colour.
func TestNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	out, errw := &screen{width: 120}, &screen{width: 120}
	runOn(t, siteLossFactory(), out, errw, "plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss")
	if strings.Contains(out.String()+errw.String(), "\x1b[") {
		t.Errorf("colour with NO_COLOR:\n%q", out.String()+errw.String())
	}
	if !strings.Contains(out.String(), "┌") {
		t.Errorf("no table on a terminal with NO_COLOR:\n%s", out)
	}
}

// A pipe gets plain text: no colour, no symbol, no table line. The golden
// files hold the exact bytes.
func TestPipeIsPlain(t *testing.T) {
	var out, errw bytes.Buffer
	runOn(t, siteLossFactory(), &out, &errw, "plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss")
	for _, bad := range []string{"\x1b[", "┌", "│", "✔"} {
		if strings.Contains(out.String()+errw.String(), bad) {
			t.Errorf("a pipe gets %q:\n%s%s", bad, out.String(), errw.String())
		}
	}
}

// A run on a terminal marks each step done with a green ✔, and its state
// in tack status in colour; a refusal is red on stderr.
func TestRunAndStatusOnATerminal(t *testing.T) {
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go (&testenv.Operator{Clients: f.clients, Sites: []string{"a", "b"}}).Run(ctx)
	out, errw := &screen{width: 100}, &screen{width: 100}
	if code := runOn(t, f, out, errw, "run", "-f", "testdata/workload.yaml", "--to", "b", "--yes"); code != ExitOK {
		t.Fatalf("exit %d:\n%s%s", code, out, errw)
	}
	// tack run writes its progress lines to stdout, after the plan.
	if !strings.Contains(out.String(), ansiGreen+"✔ done"+ansiReset+"      hold-a-cluster-pg-a") {
		t.Errorf("no green ✔ done:\n%q", out)
	}
	out.Reset()
	runOn(t, f, out, errw, "status", "pg-main-20261027-101500", "-f", "testdata/workload.yaml")
	if !strings.Contains(out.String(), "State: "+ansiGreen+"Succeeded"+ansiReset) || !strings.Contains(out.String(), "["+ansiGreen+"Succeeded"+ansiReset+"]") {
		t.Errorf("status on a terminal:\n%s", out)
	}
	out.Reset()
	runOn(t, f, out, errw, "status", "-f", "testdata/workload.yaml")
	if !strings.Contains(out.String(), " "+ansiBold+"Run"+ansiReset+" ") {
		t.Errorf("the list of runs is no table:\n%s", out)
	}
	errw.Reset()
	runOn(t, f, out, errw, "status", "no-such-run", "-f", "testdata/workload.yaml")
	if !strings.HasPrefix(errw.String(), ansiRed+"tack: ") {
		t.Errorf("the error line is not red:\n%q", errw)
	}
}

// The last column wraps in a narrow terminal.
func TestBoxWraps(t *testing.T) {
	var b bytes.Buffer
	look{term: true, width: 40}.box(&b, "", []string{"Site", "Summary"}, nil, [][]string{{"a", "one two three four five six seven eight nine ten"}})
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if visible(line) > 40 {
			t.Errorf("line of %d: %q", visible(line), line)
		}
	}
	if strings.Count(b.String(), "\n") < 6 {
		t.Errorf("did not wrap:\n%s", b.String())
	}
}

// A warning note is yellow on a terminal.
func TestWarningIsYellow(t *testing.T) {
	f := siteLossFactory()
	f.clients.SetStatus("b", nil, errForbidden)
	out, errw := &screen{width: 200}, &screen{width: 200}
	runOn(t, f, out, errw, "plan", "-f", "testdata/workload.yaml", "--to", "b", "--mode", "site-loss")
	if !strings.Contains(out.String(), "  "+ansiYellow+"warning: site b: the WAL position could not be read") {
		t.Errorf("no yellow warning:\n%s", out)
	}
}

var errForbidden = &forbidden{}

type forbidden struct{}

func (*forbidden) Error() string { return `pods "pg-b-1" is forbidden` }
