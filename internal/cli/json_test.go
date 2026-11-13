package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/takodotid/tack/internal/testenv"
)

// generic reads YAML or JSON into plain maps, so the two compare.
func generic(t *testing.T, doc string) any {
	t.Helper()
	j, err := yaml.YAMLToJSON([]byte(doc))
	if err != nil {
		t.Fatalf("%v:\n%s", err, doc)
	}
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// #78, [RFC#0009] Requirement 32: -o json prints what -o yaml prints, for
// the plan, one run, and the list of runs.
func TestJSONMatchesYAML(t *testing.T) {
	objects := map[string][]client.Object{
		"a": testenv.Site("a", "a", 2),
		"b": append(testenv.Site("b", "a", 0), stoppedRun(t, "pg-main-earlier"), stoppedRun(t, "pg-main-other")),
	}
	f := newFactory(objects)
	for _, args := range [][]string{
		{"plan", "-f", "testdata/workload.yaml", "--to", "b"},
		{"status", "pg-main-earlier", "-f", "testdata/workload.yaml"},
	} {
		var yo, jo, errw bytes.Buffer
		code := runOn(t, f, &yo, &errw, append(args, "-o", "yaml")...)
		jcode := runOn(t, f, &jo, &errw, append(args, "-o", "json")...)
		y, j := yo.String(), jo.String()
		if code != ExitOK || jcode != ExitOK {
			t.Fatalf("%v: exit %d and %d:\n%s\n%s\n%s", args, code, jcode, y, j, errw.String())
		}
		if !json.Valid([]byte(j)) {
			t.Fatalf("%v: not JSON:\n%s", args, j)
		}
		if a, b := generic(t, y), generic(t, j); !reflect.DeepEqual(a, b) {
			t.Errorf("%v: YAML and JSON differ:\n%s\n%s", args, y, j)
		}
	}

	var out, errw bytes.Buffer
	runOn(t, f, &out, &errw, "status", "-f", "testdata/workload.yaml", "-o", "yaml")
	var docs []any
	for _, d := range strings.Split(out.String(), "\n---\n") {
		docs = append(docs, generic(t, d))
	}
	out.Reset()
	runOn(t, f, &out, &errw, "status", "-f", "testdata/workload.yaml", "-o", "json")
	var list struct {
		Kind  string `json:"kind"`
		Items []any  `json:"items"`
	}
	if err := json.Unmarshal(out.Bytes(), &list); err != nil || list.Kind != "List" {
		t.Fatalf("not a JSON List: %v\n%s", err, out.String())
	}
	if len(docs) != 2 || !reflect.DeepEqual(docs, list.Items) {
		t.Errorf("the list differs: %d YAML documents, %d JSON items", len(docs), len(list.Items))
	}
}

// A site that gave no answer: the JSON on stdout still parses, stderr names
// the site, and the command exits 1, as with -o yaml ([RFC#0009]
// Requirement 16).
func TestJSONListNamesMissingSitesOnStderr(t *testing.T) {
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": append(testenv.Site("b", "a", 0), stoppedRun(t, "pg-main-earlier"))}, "a")
	var out, errw bytes.Buffer
	code := runOn(t, f, &out, &errw, "status", "-f", "testdata/workload.yaml", "-o", "json")
	if code != ExitError || !json.Valid(out.Bytes()) || !strings.Contains(errw.String(), "site a: ") {
		t.Errorf("exit %d, stdout:\n%s\nstderr:\n%s", code, out.String(), errw.String())
	}
}

// -o takes text, yaml or json.
func TestOutputFormats(t *testing.T) {
	f := newFactory(map[string][]client.Object{"a": testenv.Site("a", "a", 2), "b": testenv.Site("b", "a", 0)})
	for _, cmd := range []string{"plan", "status"} {
		args := []string{cmd, "-f", "testdata/workload.yaml", "-o", "xml"}
		if cmd == "plan" {
			args = append(args, "--to", "b")
		}
		if out, code := run(t, f, args...); code != ExitUsage || !strings.Contains(out, "--output must be text, yaml or json") {
			t.Errorf("%s -o xml: exit %d:\n%s", cmd, code, out)
		}
	}
}
