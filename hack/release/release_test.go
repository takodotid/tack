// Package release tests the scripts and workflows of the release and of CI ([RFC#0009] Requirements 19 to 26) against
// fakes of gh and curl, which answer from fixtures.
package release

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// fakeGH answers gh api PATH [--jq EXPR] from the file named after PATH in $FAKE_GH, through jq. A file PATH.error
// holds the error that gh prints. Without either, it answers as GitHub does for a missing object.
const fakeGH = `#!/usr/bin/env bash
set -euo pipefail
[[ $1 == api ]] || { echo "fake gh: $*" >&2; exit 2; }
shift
path= expr=.
while [[ $# -gt 0 ]]; do
	case $1 in
	--paginate) ;;
	--jq) expr=$2; shift ;;
	*) path=$1 ;;
	esac
	shift
done
f=$FAKE_GH/$(printf %s "$path" | tr -c 'A-Za-z0-9.' '_')
if [[ -f $f.error ]]; then cat "$f.error" >&2; exit 1; fi
if [[ ! -f $f ]]; then echo 'gh: Not Found (HTTP 404)' >&2; exit 1; fi
jq -r "$expr" "$f"
`

// fakeCurl answers a token request with a token, or fails as curl -f does when $FAKE_TOKEN is "fail". It answers a
// manifest request with the HTTP status in $FAKE_MANIFEST, or with -D, with headers that hold that status and the
// digest in $FAKE_DIGEST. Any other download given -o writes bytes that match no pinned SHA-256.
const fakeCurl = `#!/usr/bin/env bash
headers=0 out= prev=
for a; do
	[[ $a == https://* ]] && url=$a
	[[ $a == -D ]] && headers=1
	[[ $prev == -o || $prev == -fsSLo ]] && out=$a
	prev=$a
done
case $url in
*/token\?*) [[ ${FAKE_TOKEN:-} == fail ]] && { echo "curl: (22) 403" >&2; exit 22; }; echo '{"token":"t"}' ;;
*/manifests/*)
	if [[ $headers == 1 ]]; then
		printf 'HTTP/2 %s\r\n' "$FAKE_MANIFEST"
		[[ -n ${FAKE_DIGEST:-} ]] && printf 'docker-content-digest: %s\r\n' "$FAKE_DIGEST"
		printf '\r\n'
	else
		printf %s "$FAKE_MANIFEST"
	fi
	;;
*)
	[[ -n $out ]] || { echo "fake curl: $url" >&2; exit 2; }
	printf 'not the pinned download\n' >"$out"
	;;
esac
`

// fixtureName is the file name that fakeGH reads for an API path.
func fixtureName(path string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' {
			return r
		}
		return '_'
	}, path)
}

// need skips the test when a command is missing, except in CI, which must run it.
func need(t *testing.T, cmd string) {
	t.Helper()
	if _, err := exec.LookPath(cmd); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not installed; CI must run this test", cmd)
		}
		t.Skipf("%s is not installed", cmd)
	}
}

// run runs a script, a path from the repository's root, with the fakes first in PATH, the fixtures given, and the
// environment given. It returns the combined output and whether the script exited 0.
func run(t *testing.T, fixtures map[string]string, env []string, script string, args ...string) (string, bool) {
	t.Helper()
	need(t, "jq")
	bin, data := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{"gh": fakeGH, "curl": fakeCurl} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range fixtures {
		name := fixtureName(strings.TrimSuffix(path, ".error"))
		if strings.HasSuffix(path, ".error") {
			name += ".error"
		}
		if err := os.WriteFile(filepath.Join(data, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join("..", "..", script), args...)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_GH="+data)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatal(err)
	}
	return string(out), err == nil
}

// readYAML reads a file of the repository, a path from its root, into v.
func readYAML(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// ciJobs returns the keys of the jobs of ci.yaml.
func ciJobs(t *testing.T) []string {
	t.Helper()
	var ci struct{ Jobs map[string]any }
	readYAML(t, ".github/workflows/ci.yaml", &ci)
	if len(ci.Jobs) == 0 {
		t.Fatal("ci.yaml has no jobs")
	}
	return slices.Sorted(maps.Keys(ci.Jobs))
}

// jobsJSON is the jobs of a run, each with the conclusion given, or success.
func jobsJSON(jobs []string, conclusions map[string]string) string {
	var b strings.Builder
	b.WriteString(`{"jobs":[`)
	for i, j := range jobs {
		c := conclusions[j]
		if c == "" {
			c = "success"
		}
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"` + j + `","conclusion":"` + c + `"}`)
	}
	b.WriteString("]}")
	return b.String()
}

// The release starts only for a signed tag vX.Y.Z on main whose ci run on main succeeded in every job ([RFC#0009]
// Requirement 19). Found in a review (#33): any v* tag published a release.
func TestGate(t *testing.T) {
	const (
		repo   = "o/r"
		tag    = "v1.3.0"
		commit = "c0ffee"
		runs   = "repos/o/r/actions/workflows/ci.yaml/runs?head_sha=c0ffee&event=push&branch=main&per_page=100"
		jobs1  = "repos/o/r/actions/runs/1/jobs?filter=latest&per_page=100"
		jobs2  = "repos/o/r/actions/runs/2/jobs?filter=latest&per_page=100"
	)
	jobs := ciJobs(t)
	last := jobs[len(jobs)-1]
	base := func() map[string]string {
		return map[string]string{
			"repos/o/r/git/ref/tags/v1.3.0":   `{"object":{"type":"tag","sha":"7a9"}}`,
			"repos/o/r/git/tags/7a9":          `{"tag":"v1.3.0","object":{"sha":"c0ffee"},"verification":{"verified":true,"reason":"valid"}}`,
			"repos/o/r/compare/c0ffee...main": `{"status":"ahead"}`,
			runs:                              `{"workflow_runs":[{"id":1,"status":"completed","conclusion":"success"}]}`,
			jobs1:                             jobsJSON(jobs, nil),
		}
	}
	for _, tt := range []struct {
		name   string
		tag    string
		change map[string]string
		ok     bool
		want   string
	}{
		{name: "released", ok: true, want: "every job successful"},
		{name: "commit is main", change: map[string]string{"repos/o/r/compare/c0ffee...main": `{"status":"identical"}`}, ok: true},
		{name: "a failed run, then a successful one", ok: true, change: map[string]string{
			runs:  `{"workflow_runs":[{"id":2,"status":"completed","conclusion":"failure"},{"id":1,"status":"completed","conclusion":"success"}]}`,
			jobs2: jobsJSON(jobs, map[string]string{last: "failure"}),
		}},
		{name: "not vX.Y.Z", tag: "v1.3", want: "is not vX.Y.Z"},
		{name: "pre-release", tag: "v1.3.0-rc.1", want: "is not vX.Y.Z"},
		{name: "no such tag", tag: "v1.3.1", want: "cannot read tag v1.3.1"},
		{name: "lightweight tag", change: map[string]string{"repos/o/r/git/ref/tags/v1.3.0": `{"object":{"type":"commit","sha":"c0ffee"}}`}, want: "not annotated"},
		{name: "unsigned", change: map[string]string{"repos/o/r/git/tags/7a9": `{"tag":"v1.3.0","object":{"sha":"c0ffee"},"verification":{"verified":false,"reason":"unsigned"}}`}, want: "does not verify the signature of tag v1.3.0: unsigned"},
		{name: "unknown key", change: map[string]string{"repos/o/r/git/tags/7a9": `{"tag":"v1.3.0","object":{"sha":"c0ffee"},"verification":{"verified":false,"reason":"unknown_key"}}`}, want: "unknown_key"},
		{name: "tag object of another name", change: map[string]string{"repos/o/r/git/tags/7a9": `{"tag":"v1.2.9","object":{"sha":"c0ffee"},"verification":{"verified":true,"reason":"valid"}}`}, want: "is named v1.2.9"},
		{name: "another commit", change: map[string]string{"repos/o/r/git/tags/7a9": `{"tag":"v1.3.0","object":{"sha":"beef"},"verification":{"verified":true,"reason":"valid"}}`}, want: "points at beef, not at c0ffee"},
		{name: "not on main", change: map[string]string{"repos/o/r/compare/c0ffee...main": `{"status":"diverged"}`}, want: "is not on main: main is diverged"},
		{name: "ahead of main", change: map[string]string{"repos/o/r/compare/c0ffee...main": `{"status":"behind"}`}, want: "is not on main"},
		{name: "compare fails", change: map[string]string{"repos/o/r/compare/c0ffee...main.error": "gh: Server Error (HTTP 500)"}, want: "cannot compare"},
		{name: "ci running", change: map[string]string{runs: `{"workflow_runs":[{"id":1,"status":"in_progress","conclusion":null}]}`}, want: "has not finished"},
		{name: "ci failed", change: map[string]string{runs: `{"workflow_runs":[{"id":1,"status":"completed","conclusion":"failure"}]}`}, want: "no ci run"},
		{name: "ci cancelled", change: map[string]string{runs: `{"workflow_runs":[{"id":1,"status":"completed","conclusion":"cancelled"}]}`}, want: "no ci run"},
		{name: "no ci run", change: map[string]string{runs: `{"workflow_runs":[]}`}, want: "no ci run"},
		{name: "ci runs unreadable", change: map[string]string{runs + ".error": "gh: Bad credentials (HTTP 401)"}, want: "cannot list the ci runs"},
		{name: "a job skipped", change: map[string]string{jobs1: jobsJSON(jobs, map[string]string{last: "skipped"})}, want: "not successful: " + last},
		{name: "a job missing", change: map[string]string{jobs1: jobsJSON(jobs[:len(jobs)-1], nil)}, want: "not successful: " + last},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixtures := base()
			for k, v := range tt.change {
				if !strings.HasSuffix(k, ".error") {
					fixtures[k] = v
					continue
				}
				delete(fixtures, strings.TrimSuffix(k, ".error"))
				fixtures[k] = v
			}
			name := tt.tag
			if name == "" {
				name = tag
			}
			out, ok := run(t, fixtures, nil, "hack/release/gate.sh", repo, name, commit)
			if ok != tt.ok {
				t.Fatalf("exit 0: %v, want %v: %s", ok, tt.ok, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output lacks %q: %s", tt.want, out)
			}
		})
	}
	// gate.sh reads the jobs from ci.yaml: each one counts.
	for _, job := range jobs {
		t.Run("without "+job, func(t *testing.T) {
			fixtures := base()
			fixtures[jobs1] = jobsJSON(slices.DeleteFunc(slices.Clone(jobs), func(j string) bool { return j == job }), nil)
			out, ok := run(t, fixtures, nil, "hack/release/gate.sh", repo, tag, commit)
			if ok || !strings.Contains(out, "not successful: "+job) {
				t.Errorf("exit 0: %v, want a refusal that names %s: %s", ok, job, out)
			}
		})
	}
}

// absent.sh refuses a published version, and one whose absence the registry or GitHub does not confirm ([RFC#0009]
// Requirement 22).
func TestAbsent(t *testing.T) {
	const release = "repos/o/r/releases/tags/v1.3.0"
	const chart = "ghcr.io/o/charts/tack:1.3.0"
	for _, tt := range []struct {
		name     string
		args     []string
		fixtures map[string]string
		env      []string
		ok       bool
		want     string
	}{
		{name: "chart absent", args: []string{"oci", chart}, env: []string{"FAKE_MANIFEST=404"}, ok: true, want: "absent"},
		{name: "chart published", args: []string{"oci", chart}, env: []string{"FAKE_MANIFEST=200"}, want: "published already"},
		{name: "registry fails", args: []string{"oci", chart}, env: []string{"FAKE_MANIFEST=500"}, want: "HTTP 500, so its absence cannot be told"},
		{name: "registry refuses", args: []string{"oci", chart}, env: []string{"FAKE_MANIFEST=401"}, want: "cannot be told"},
		{name: "no token", args: []string{"oci", chart}, env: []string{"FAKE_TOKEN=fail", "FAKE_MANIFEST=404"}, want: "no pull token"},
		{name: "no tag", args: []string{"oci", "ghcr.io/o/charts/tack"}, env: []string{"FAKE_MANIFEST=404"}, want: "is not HOST/NAME:TAG"},
		{name: "release absent", args: []string{"release", "o/r", "v1.3.0"}, ok: true, want: "absent"},
		{name: "release published", args: []string{"release", "o/r", "v1.3.0"}, fixtures: map[string]string{release: `{"html_url":"https://example/v1.3.0"}`}, want: "published already"},
		{name: "GitHub fails", args: []string{"release", "o/r", "v1.3.0"}, fixtures: map[string]string{release + ".error": "gh: Server Error (HTTP 502)"}, want: "cannot be told"},
		{name: "GitHub refuses", args: []string{"release", "o/r", "v1.3.0"}, fixtures: map[string]string{release + ".error": "gh: Bad credentials (HTTP 401)"}, want: "cannot be told"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, ok := run(t, tt.fixtures, tt.env, "hack/release/absent.sh", tt.args...)
			if ok != tt.ok {
				t.Fatalf("exit 0: %v, want %v: %s", ok, tt.ok, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output lacks %q: %s", tt.want, out)
			}
		})
	}
}

// digest.sh prints the digest that the registry gives for a tag, and fails on anything else.
func TestDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("9", 64)
	for _, tt := range []struct {
		name string
		args []string
		env  []string
		ok   bool
		want string
	}{
		{"pushed", []string{"ghcr.io/o/charts/tack:1.3.0"}, []string{"FAKE_MANIFEST=200", "FAKE_DIGEST=" + digest}, true, digest + "\n"},
		{"absent", []string{"ghcr.io/o/charts/tack:1.3.0"}, []string{"FAKE_MANIFEST=404", "FAKE_DIGEST=" + digest}, false, "HTTP 404"},
		{"no digest", []string{"ghcr.io/o/charts/tack:1.3.0"}, []string{"FAKE_MANIFEST=200", "FAKE_DIGEST="}, false, "no sha256 digest"},
		{"other algorithm", []string{"ghcr.io/o/charts/tack:1.3.0"}, []string{"FAKE_MANIFEST=200", "FAKE_DIGEST=sha512:ab"}, false, "no sha256 digest"},
		{"no token", []string{"ghcr.io/o/charts/tack:1.3.0"}, []string{"FAKE_TOKEN=fail", "FAKE_MANIFEST=200", "FAKE_DIGEST=" + digest}, false, "no pull token"},
		{"no tag", []string{"ghcr.io/o/charts/tack"}, []string{"FAKE_MANIFEST=200", "FAKE_DIGEST=" + digest}, false, "is not HOST/NAME:TAG"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, ok := run(t, nil, tt.env, "hack/release/digest.sh", tt.args...)
			if ok != tt.ok {
				t.Fatalf("exit 0: %v, want %v: %s", ok, tt.ok, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output lacks %q: %s", tt.want, out)
			}
		})
	}
}

// pinned.sh refuses an action that is not pinned by commit SHA ([RFC#0009] Requirement 25), and passes the repository.
func TestPinned(t *testing.T) {
	sha := strings.Repeat("0", 40)
	for _, tt := range []struct {
		uses string
		ok   bool
	}{
		{"actions/checkout@" + sha + " # v7.0.1", true},
		{"actions/checkout@" + sha, true},
		{"./.github/actions/local", true},
		{"actions/checkout@v7", false},
		{"actions/checkout@main", false},
		{"actions/checkout@" + sha[:39], false},
		{"docker://alpine:3", false},
	} {
		root := t.TempDir()
		dir := filepath.Join(root, ".github", "workflows")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		workflow := "jobs:\n  a:\n    steps:\n      - uses: " + tt.uses + "\n"
		if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(workflow), 0o644); err != nil {
			t.Fatal(err)
		}
		out, ok := run(t, nil, nil, "hack/release/pinned.sh", root)
		if ok != tt.ok {
			t.Errorf("uses: %s: exit 0: %v, want %v: %s", tt.uses, ok, tt.ok, out)
		}
	}
	if out, ok := run(t, nil, nil, "hack/release/pinned.sh"); !ok {
		t.Errorf("the repository: %s", out)
	}
}

// install.sh refuses a download whose SHA-256 is not the one pinned, and installs nothing ([RFC#0009] Requirement 25).
// Found in a review (#35, rows 25 and 46): the release's helm was the latest one, and the e2e checked its tools
// against checksums fetched from the same place.
func TestInstallChecksSHA256(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("install.sh pins Linux on amd64 and arm64 only")
	}
	b, err := os.ReadFile("../tools/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"kind", "kubectl", "flux", "helm", "cosign"} {
		for _, arch := range []string{"amd64", "arm64"} {
			if !regexp.MustCompile(`(?m)^` + tool + ` +v\d+\.\d+\.\d+ +` + arch + ` +[0-9a-f]{64}$`).Match(b) {
				t.Errorf("install.sh pins no SHA-256 for %s on %s", tool, arch)
			}
		}
	}
	for _, tt := range []struct {
		tools []string
		want  string
	}{
		{nil, "kind-linux-"},
		{[]string{"helm"}, "helm-v4.3.1-linux-"},
		{[]string{"cosign"}, "cosign-linux-"},
		{[]string{"kubectl"}, "/kubectl does not match its pinned SHA-256"},
		{[]string{"flux"}, "flux_2.10.0_linux_"},
		{[]string{"zip"}, "unknown tool zip"},
	} {
		dest := t.TempDir()
		out, ok := run(t, nil, nil, "hack/tools/install.sh", append([]string{dest}, tt.tools...)...)
		if ok || !strings.Contains(out, tt.want) {
			t.Errorf("%v: exit 0: %v, want a refusal with %q: %s", tt.tools, ok, tt.want, out)
		}
		if entries, _ := os.ReadDir(dest); len(entries) > 0 {
			t.Errorf("%v: installed %d files", tt.tools, len(entries))
		}
	}
}

// install.sh refuses a tool whose two lines pin different versions, such as after an update of one line only, and
// installs nothing.
func TestInstallRefusesTwoVersions(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("install.sh pins Linux only")
	}
	b, err := os.ReadFile("../tools/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^(kind +)v0\.\d+\.\d+( +arm64 )`)
	if !line.Match(b) {
		t.Fatal("install.sh pins no kind for arm64")
	}
	script := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(script, line.ReplaceAll(b, []byte("${1}v0.1.0${2}")), 0o755); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	out, err := exec.Command(script, dest, "kind").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "install.sh: the lines of kind pin versions 'v0.1.0 v0.") {
		t.Errorf("exit %v, want a refusal: %s", err, out)
	}
	if entries, _ := os.ReadDir(dest); len(entries) > 0 {
		t.Errorf("installed %d files", len(entries))
	}
}

// #66. Renovate did not update the tools' pins. renovate.json's custom managers read every line of install.sh: the
// version of each, and the SHA-256 of kind, flux and cosign, whose downloads are GitHub release assets. kubectl and
// helm have their SHA-256 copied by hand: no manager reads it, so Renovate never writes something else there.
func TestRenovateReadsEveryPin(t *testing.T) {
	var cfg struct {
		CustomManagers []struct {
			ManagerFilePatterns []string
			MatchStrings        []string
			DatasourceTemplate  string
		}
	}
	readYAML(t, "renovate.json", &cfg)
	b, err := os.ReadFile("../tools/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	pins := regexp.MustCompile(`(?m)^(\w+) +(v\S+) +(amd64|arm64) +([0-9a-f]{64})$`).FindAllStringSubmatch(string(b), -1)
	if len(pins) != 10 {
		t.Fatalf("install.sh has %d pin lines, want 10", len(pins))
	}
	digests := map[string]bool{"kind": true, "flux": true, "cosign": true, "kubectl": false, "helm": false}
	for _, pin := range pins {
		var found []string
		for _, m := range cfg.CustomManagers {
			if len(m.ManagerFilePatterns) != 1 || m.ManagerFilePatterns[0] != `/^hack/tools/install\.sh$/` {
				continue
			}
			for _, ms := range m.MatchStrings {
				re := regexp.MustCompile(ms)
				g := re.FindStringSubmatch(pin[0])
				if g == nil {
					continue
				}
				dep, value := g[re.SubexpIndex("depName")], g[re.SubexpIndex("currentValue")]
				digest := ""
				if i := re.SubexpIndex("currentDigest"); i >= 0 {
					digest = g[i]
				}
				found = append(found, m.DatasourceTemplate)
				if dep != pin[1] || value != pin[2] || digest != map[bool]string{true: pin[4]}[digests[pin[1]]] {
					t.Errorf("%s: read as %s %s, digest %q", pin[0], dep, value, digest)
				}
			}
		}
		if len(found) != 1 {
			t.Errorf("%s: read by %d match strings (%v), want 1", pin[0], len(found), found)
		}
	}
}

// build.sh refuses a version that is not vX.Y.Z, with or without a pre-release, before it builds anything.
func TestBuildRefusesVersion(t *testing.T) {
	for _, v := range []string{"1.3.0", "v1.3", "v1.3.0+build", "v1.3.0 ", ""} {
		out, ok := run(t, nil, nil, "hack/release/build.sh", v)
		if ok || !strings.Contains(out, "is not vX.Y.Z") {
			t.Errorf("%q: exit 0: %v, want a refusal: %s", v, ok, out)
		}
	}
}

// needs is a job's needs, one job or a list.
type needs []string

func (n *needs) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*n = needs{one}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(n))
}

// job is what the tests read of a job of a workflow.
type job struct {
	Needs       needs
	Environment string
	Permissions map[string]string
	Steps       []step
}

// step is a step of a job.
type step struct {
	Uses, Run string
	With      map[string]string
}

// step returns the index of the first step whose script holds all the strings given, or -1.
func (j job) step(holds ...string) int {
	for i, s := range j.Steps {
		ok := true
		for _, h := range holds {
			ok = ok && strings.Contains(s.Run, h)
		}
		if ok {
			return i
		}
	}
	return -1
}

// uses returns the index of the first step that uses the action given, or -1.
func (j job) uses(action string) int {
	return slices.IndexFunc(j.Steps, func(s step) bool { return strings.HasPrefix(s.Uses, action+"@") })
}

// before reports whether the script holds a before b.
func before(script, a, b string) bool {
	i, j := strings.Index(script, a), strings.Index(script, b)
	return i >= 0 && j > i
}

// runsGo matches a step that runs Go on the runner. make counts: every target runs go.
var runsGo = regexp.MustCompile(`(^|[^a-z_./-])(make|go)( |$)|hack/release/build\.sh`)

// assets are the files of a release, as the workflow names them: test uploads them, publish attests and releases them.
var assets = []string{"dist/*.tar.gz", "dist/*.zip", "dist/*.cdx.json", "dist/*.tgz", "dist/failover.tako.id_sitefailovers.yaml", "dist/SHA256SUMS"}

// The release tests and builds before any push, runs Go only in jobs that can write nothing, never replaces a version,
// and signs and attests what it publishes ([RFC#0009] Requirements 19 to 24). Found in a review (#33, #35 row 24): the
// release built and published in one job with write access, and signed nothing.
func TestReleaseWorkflow(t *testing.T) {
	var w struct {
		Permissions map[string]string
		Concurrency struct {
			CancelInProgress bool `json:"cancel-in-progress"`
		}
		Jobs map[string]job
	}
	readYAML(t, ".github/workflows/release.yaml", &w)
	writes := func(p map[string]string) []string {
		var got []string
		for k, v := range p {
			if v != "read" && v != "none" {
				got = append(got, k+": "+v)
			}
		}
		slices.Sort(got)
		return got
	}
	if got := writes(w.Permissions); len(got) > 0 {
		t.Errorf("the workflow's permissions grant %v to every job", got)
	}
	if w.Concurrency.CancelInProgress {
		t.Error("a new run cancels a release in progress")
	}
	// after reports whether job a needs job b, directly or not.
	var after func(a, b string, seen map[string]bool) bool
	after = func(a, b string, seen map[string]bool) bool {
		if seen[a] {
			return false
		}
		seen[a] = true
		for _, n := range w.Jobs[a].Needs {
			if n == b || after(n, b, seen) {
				return true
			}
		}
		return false
	}
	gate, ok := w.Jobs["gate"]
	if !ok || len(gate.Needs) > 0 {
		t.Fatal("no first job gate")
	}
	if gate.Environment != "release" {
		t.Errorf("gate runs in the environment %q, want release", gate.Environment)
	}
	if gate.step("hack/release/gate.sh") < 0 {
		t.Error("gate does not run gate.sh")
	}
	test, ok := w.Jobs["test"]
	if !ok {
		t.Fatal("no job test")
	}
	build := test.step("hack/release/build.sh")
	if test.step("make verify test") < 0 || build < 0 {
		t.Error("job test does not run make verify test and hack/release/build.sh")
	}
	if upload := test.uses("actions/upload-artifact"); upload < build {
		t.Errorf("job test uploads at step %d, before build.sh at step %d", upload, build)
	} else if got := strings.Fields(test.Steps[upload].With["path"]); !slices.Equal(got, assets) {
		t.Errorf("job test uploads %v, want %v", got, assets)
	}
	for name, job := range w.Jobs {
		if name != "gate" && !after(name, "gate", map[string]bool{}) {
			t.Errorf("job %s does not need gate", name)
		}
		w := writes(job.Permissions)
		if len(w) == 0 {
			continue
		}
		if !after(name, "test", map[string]bool{}) {
			t.Errorf("job %s holds %v and does not need test", name, w)
		}
		for _, s := range job.Steps {
			if strings.HasPrefix(s.Uses, "actions/setup-go@") || runsGo.MatchString(s.Run) {
				t.Errorf("job %s holds %v and runs Go: %s%s", name, w, s.Uses, s.Run)
			}
		}
	}

	// Requirement 22: gate checks both, and each push checks its own right before it.
	if gate.step("absent.sh oci", "absent.sh release") < 0 {
		t.Error("gate does not check that the version is absent")
	}
	publish := w.Jobs["publish"]
	chart := publish.step("helm push")
	release := publish.step("gh release create")
	if chart < 0 || release < 0 {
		t.Fatalf("publish: the chart's push at step %d, the release at %d", chart, release)
	}
	if !before(publish.Steps[chart].Run, "absent.sh oci", "helm push") {
		t.Error("publish: the chart's push is not checked right before it")
	}
	if !before(publish.Steps[release].Run, "absent.sh release", "gh release create") {
		t.Error("publish: the GitHub release is not checked right before it")
	}
	if chart > release {
		t.Errorf("publish: the release at step %d comes before the chart's push at %d", release, chart)
	}
	// Requirement 23: the assets as test built them, every asset attested and SHA256SUMS signed before the first push,
	// the chart signed after its push, and every asset released with the signature of SHA256SUMS.
	if check := publish.step("sha256sum --check --strict SHA256SUMS"); check < 0 || check > publish.uses("actions/attest") {
		t.Errorf("publish: SHA256SUMS checked at step %d, the attestations at %d", check, publish.uses("actions/attest"))
	}
	attest := publish.uses("actions/attest")
	if attest < 0 || attest > chart {
		t.Fatalf("publish: attestations at step %d, the chart's push at %d", attest, chart)
	}
	if got := strings.Fields(publish.Steps[attest].With["subject-path"]); !slices.Equal(got, assets) {
		t.Errorf("publish attests %v, want %v", got, assets)
	}
	if sign := publish.step("cosign sign-blob", "--bundle dist/SHA256SUMS.sigstore.json dist/SHA256SUMS"); sign < 0 || sign > chart {
		t.Errorf("publish: SHA256SUMS signed at step %d, the chart's push at %d", sign, chart)
	}
	if !before(publish.Steps[chart].Run, "helm push", "cosign sign --yes") {
		t.Error("publish: the chart is not signed after its push")
	}
	for _, a := range append(slices.Clone(assets), "SHA256SUMS.sigstore.json") {
		if !strings.Contains(publish.Steps[release].Run, " "+strings.TrimPrefix(a, "dist/")) {
			t.Errorf("publish: the GitHub release lacks %s", a)
		}
	}
	verify := publish.step("cosign verify ", "cosign verify-blob", "gh attestation verify")
	if verify < release {
		t.Errorf("publish: the release verified at step %d, before or without its creation at %d", verify, release)
	}
}

// CI runs the release's build and the lint on each pull request and each push to main, and no later commit of main
// cancels a commit's run ([RFC#0009] Requirements 19, 20 and 26).
func TestCIWorkflow(t *testing.T) {
	var w struct {
		Concurrency struct {
			Group            string
			CancelInProgress string `json:"cancel-in-progress"`
		}
		Jobs map[string]job
	}
	readYAML(t, ".github/workflows/ci.yaml", &w)
	if !strings.Contains(w.Concurrency.Group, "github.sha") || !strings.Contains(w.Concurrency.CancelInProgress, "pull_request") {
		t.Errorf("concurrency %+v: a later commit of main can cancel a commit's run", w.Concurrency)
	}
	for name, script := range map[string]string{"test": "hack/release/build.sh", "lint": "make lint", "e2e": "make e2e"} {
		if w.Jobs[name].step(script) < 0 {
			t.Errorf("job %s does not run %s", name, script)
		}
	}
}

// digest is an image named by the digest of its index, with or without a tag.
var digest = regexp.MustCompile(`^[a-z0-9./_-]+(:[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}$`)

// Every image that the e2e pulls or builds from is named by its digest, and the fixtures name the images that the nodes
// pull ([RFC#0009] Requirement 25). Found in a review (#35, row 46): images and base images were named by tag only.
func TestE2EImagesPinned(t *testing.T) {
	need(t, "bash")
	out, err := exec.Command("bash", "-c", `source ../../test/e2e/env/lib.sh &&
		printf '%s\n' "$KIND_NODE_IMAGE" "$FLUX_SOURCE_IMAGE" "$FLUX_KUSTOMIZE_IMAGE" "$CNPG_IMAGE" "$ENVOY_IMAGE" &&
		printf 'prepull %s\n' "${PREPULL_IMAGES[@]}"`).CombinedOutput()
	if err != nil {
		t.Fatalf("lib.sh: %v: %s", err, out)
	}
	var prepull []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		image, ok := strings.CutPrefix(line, "prepull ")
		if ok {
			prepull = append(prepull, image)
		}
		if !digest.MatchString(image) {
			t.Errorf("lib.sh names the image %q, not by digest", image)
		}
	}
	field := regexp.MustCompile(`(?m)^\s*-?\s*(image|imageName):\s*(\S+)\s*$`)
	from := regexp.MustCompile(`(?m)^FROM\s+(?:--\S+\s+)*(\S+)(?:\s+AS\s+(\S+))?`)
	err = filepath.WalkDir("../../test/e2e", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		switch {
		case strings.HasSuffix(path, ".yaml"):
			for _, m := range field.FindAllStringSubmatch(string(b), -1) {
				if !digest.MatchString(m[2]) {
					t.Errorf("%s names the image %s, not by digest", path, m[2])
				} else if !slices.Contains(prepull, m[2]) {
					t.Errorf("%s names the image %s, which PREPULL_IMAGES lacks", path, m[2])
				}
			}
		case filepath.Base(path) == "Dockerfile":
			stages := map[string]bool{}
			for _, m := range from.FindAllStringSubmatch(string(b), -1) {
				if !stages[m[1]] && !digest.MatchString(m[1]) {
					t.Errorf("%s builds from %s, not by digest", path, m[1])
				}
				stages[m[2]] = m[2] != ""
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
