//go:build e2e

// Package e2e runs tack against two kind clusters with Flux, CloudNativePG,
// Strait and a local Git server. test/e2e/env/up.sh builds them; test/e2e/run.sh
// runs everything. The scenarios run in order on one pair of clusters, the
// destructive ones last.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	repo       = mustAbs("../..")
	work       = envOr("TACK_E2E_WORKDIR", filepath.Join(repo, ".e2e"))
	kubeconfig = filepath.Join(work, "kubeconfig")
	tackBin    = filepath.Join(work, "tack")
	workload   = filepath.Join(work, "workload.yaml")
	gitDir     = filepath.Join(work, "gitops-clone")
)

const (
	ns           = "db"
	ledgerNS     = "tack-system"
	gitContainer = "tack-e2e-git"
	exitStopped  = 3
	exitWaiting  = 4
	exitFault    = 42
)

func node(site string) string    { return "tack-e2e-" + site + "-control-plane" }
func context(site string) string { return "kind-tack-e2e-" + site }
func clusterName(site string) string {
	return "pg-" + site
}
func poolerName(site string) string { return "pg-" + site + "-pooler-rw" }

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		panic(err)
	}
	return a
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// setup builds tack with the fault hook, installs the CRD and the ledger
// namespace at both sites, and writes the Workload.
func setup() error {
	build := exec.Command("go", "build", "-tags", "e2efault", "-o", tackBin, "./cmd/tack")
	build.Dir = repo
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("build tack: %w", err)
	}
	for _, s := range []string{"a", "b"} {
		if out, err := kubectlE(s, "apply", "-f", filepath.Join(repo, "config/crd")); err != nil {
			return fmt.Errorf("site %s: apply CRD: %v: %s", s, err, out)
		}
		out, _ := kubectlE(s, "create", "namespace", ledgerNS, "--dry-run=client", "-o", "yaml")
		if o, err := kubectlIn(s, out, "apply", "-f", "-"); err != nil {
			return fmt.Errorf("site %s: ledger namespace: %v: %s", s, err, o)
		}
	}
	url, err := gitURL()
	if err != nil {
		return err
	}
	// A clone left by an earlier environment follows another repository.
	if err := os.RemoveAll(gitDir); err != nil {
		return err
	}
	w := fmt.Sprintf(workloadTemplate, url)
	return os.WriteFile(workload, []byte(w), 0o644)
}

// gitURL reads the Git URL that up.sh wrote into the work dir's env file.
func gitURL() (string, error) {
	b, err := os.ReadFile(filepath.Join(work, "env"))
	if err != nil {
		return "", fmt.Errorf("read the env file that up.sh wrote: %w", err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "export TACK_E2E_GIT_URL="); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("no TACK_E2E_GIT_URL in %s", filepath.Join(work, "env"))
}

// The Pooler instances at site b use a field mapping (b); every other
// field is found in its manifest (a).
const workloadTemplate = `apiVersion: failover.tako.id/v1alpha1
kind: Workload
metadata:
  name: pg
spec:
  driver: cnpg
  sites:
  - name: a
    kubeContext: kind-tack-e2e-a
    cnpg:
      cluster: {namespace: db, name: pg-a}
      pooler: {namespace: db, name: pg-a-pooler-rw, instances: 1}
  - name: b
    kubeContext: kind-tack-e2e-b
    cnpg:
      cluster: {namespace: db, name: pg-b}
      pooler: {namespace: db, name: pg-b-pooler-rw, instances: 1}
  git:
    url: %s
    branch: main
    author: {name: tack e2e, email: tack-e2e@example.invalid}
    fields:
    - site: b
      object: {kind: Pooler, namespace: db, name: pg-b-pooler-rw}
      field: spec.instances
      file: clusters/b/pooler.yaml
      path: spec.instances
    find:
      paths:
      - {site: a, path: clusters/a}
      - {site: b, path: clusters/b}
  timeouts: {demotion: 6m, promotion: 8m, flux: 4m}
`

// tack runs the binary and returns its output and exit code.
func tack(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(tackBin, append([]string{"--kubeconfig", kubeconfig}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("tack %v: %v", args, err)
	}
	t.Logf("$ tack %s  (exit %d)\n%s", strings.Join(args, " "), code, out.String())
	return out.String(), code
}

func tackOK(t *testing.T, args ...string) string {
	t.Helper()
	out, code := tack(t, nil, args...)
	if code != 0 {
		t.Fatalf("tack %v: exit %d", args, code)
	}
	return out
}

func kubectlE(site string, args ...string) (string, error) {
	return kubectlIn(site, "", args...)
}

func kubectlIn(site, stdin string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig, "--context", context(site)}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func kubectl(t *testing.T, site string, args ...string) string {
	t.Helper()
	out, err := kubectlE(site, args...)
	if err != nil {
		t.Fatalf("kubectl %s %v: %v\n%s", site, args, err, out)
	}
	return out
}

func jsonpath(t *testing.T, site, kind, name, path string) string {
	t.Helper()
	return strings.TrimSpace(kubectl(t, site, "-n", ns, "get", kind, name, "-o", "jsonpath="+path))
}

func docker(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
}

// eventually polls f until it returns "" or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, f func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		if last = f(); last == "" {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("%s: not after %s: %s", what, timeout, last)
}

// assertPrimary checks the whole end state of a switch to site to.
func assertPrimary(t *testing.T, to, from string) {
	t.Helper()
	toName := clusterName(to)
	eventually(t, 5*time.Minute, "site "+to+" is a healthy primary", func() string {
		p := jsonpath(t, to, "cluster", toName, "{.status.phase}")
		ip := jsonpath(t, to, "cluster", toName, "{.status.instancesReportedState.*.isPrimary}")
		if p != "Cluster in healthy state" || !strings.Contains(ip, "true") {
			return fmt.Sprintf("phase %q, isPrimary %q", p, ip)
		}
		return ""
	})
	for _, s := range []string{to, from} {
		if got := jsonpath(t, s, "cluster", clusterName(s), "{.spec.replica.primary}"); got != toName {
			t.Errorf("site %s declares %q, want %q", s, got, toName)
		}
		if got := jsonpath(t, s, "cluster", clusterName(s), "{.spec.replica.promotionToken}"); got != "" {
			t.Errorf("site %s keeps a promotion token", s)
		}
		for _, kind := range []string{"cluster", "pooler"} {
			name := clusterName(s)
			if kind == "pooler" {
				name = poolerName(s)
			}
			ann := jsonpath(t, s, kind, name, "{.metadata.annotations}")
			if strings.Contains(ann, "kustomize.toolkit.fluxcd.io/reconcile") || strings.Contains(ann, "failover.tako.id/held-by") {
				t.Errorf("site %s: %s %s is still held: %s", s, kind, name, ann)
			}
		}
	}
	if got := jsonpath(t, to, "pooler", poolerName(to), "{.spec.instances}"); got != "1" {
		t.Errorf("site %s Pooler instances %q, want 1", to, got)
	}
	if got := jsonpath(t, from, "pooler", poolerName(from), "{.spec.instances}"); got != "0" {
		t.Errorf("site %s Pooler instances %q, want 0", from, got)
	}
}

// assertGitMatches checks that Git declares the primary and that Flux at
// each site applied Git's head.
func assertGitMatches(t *testing.T, to string, sites ...string) string {
	t.Helper()
	head := gitPull(t)
	for _, s := range []string{"a", "b"} {
		b, err := os.ReadFile(filepath.Join(gitDir, "clusters", s, "cluster.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "primary: "+clusterName(to)) {
			t.Errorf("Git at clusters/%s/cluster.yaml does not declare %s", s, clusterName(to))
		}
		if strings.Contains(string(b), "promotionToken") {
			t.Errorf("Git at clusters/%s/cluster.yaml holds a promotion token", s)
		}
	}
	for _, s := range sites {
		rev := strings.TrimSpace(kubectl(t, s, "-n", "flux-system", "get", "kustomization", "site-"+s, "-o", "jsonpath={.status.lastAppliedRevision}"))
		if !strings.HasSuffix(rev, head) {
			t.Errorf("Flux at site %s applied %s, Git head is %s", s, rev, head)
		}
	}
	return head
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.invalid",
		"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitPull refreshes the local clone and returns the head commit.
func gitPull(t *testing.T) string {
	t.Helper()
	url, err := gitURL()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gitDir); err != nil {
		gitCmd(t, work, "clone", "-q", url, gitDir)
	}
	gitCmd(t, gitDir, "fetch", "-q", "origin")
	gitCmd(t, gitDir, "reset", "-q", "--hard", "origin/main")
	return gitCmd(t, gitDir, "rev-parse", "HEAD")
}

// latestRun returns the newest SiteFailover at a site.
func latestRun(t *testing.T, site string) map[string]any {
	t.Helper()
	out := kubectl(t, site, "-n", ledgerNS, "get", "sitefailovers", "--sort-by=.metadata.creationTimestamp", "-o", "json")
	var l struct{ Items []map[string]any }
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Items) == 0 {
		t.Fatalf("no SiteFailover at site %s", site)
	}
	return l.Items[len(l.Items)-1]
}

func runName(sf map[string]any) string { return sf["metadata"].(map[string]any)["name"].(string) }

func runStatus(sf map[string]any) (state, reason string) {
	st, _ := sf["status"].(map[string]any)
	state, _ = st["state"].(string)
	reason, _ = st["reason"].(string)
	return
}

func stepState(sf map[string]any, id string) string {
	st, _ := sf["status"].(map[string]any)
	steps, _ := st["steps"].([]any)
	for _, s := range steps {
		m := s.(map[string]any)
		if m["id"] == id {
			return m["state"].(string)
		}
	}
	return ""
}

// psql runs SQL on the primary instance of a site.
func psql(t *testing.T, site, sql string) string {
	t.Helper()
	pod := jsonpath(t, site, "cluster", clusterName(site), "{.status.currentPrimary}")
	return strings.TrimSpace(kubectl(t, site, "-n", ns, "exec", pod, "-c", "postgres", "--", "psql", "-U", "postgres", "-d", "app", "-tAc", sql))
}

func TestPlannedSwitchover(t *testing.T) {
	psql(t, "a", "create table if not exists marks (id text primary key)")
	psql(t, "a", "insert into marks values ('before-switchover') on conflict do nothing")

	out := tackOK(t, "plan", "-f", workload, "--to", "b")
	for _, want := range []string{"demote-a", "capture-token", "promote-b", "git-commit", "verify-git-b", "Nothing was changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q", want)
		}
	}
	tackOK(t, "run", "-f", workload, "--to", "b", "--yes")
	assertPrimary(t, "b", "a")
	assertGitMatches(t, "b", "a", "b")
	if got := psql(t, "b", "select id from marks where id = 'before-switchover'"); got != "before-switchover" {
		t.Errorf("site b lacks a write made at site a before the switch: %q", got)
	}
	psql(t, "b", "insert into marks values ('after-switchover')")
	sf := latestRun(t, "b")
	if state, _ := runStatus(sf); state != "Succeeded" {
		t.Fatalf("run state %s", state)
	}
}

func TestInterruptedRunResumes(t *testing.T) {
	// The process dies right after promoting site a, before the ledger
	// records it.
	_, code := tack(t, []string{"TACK_FAULT=promote-a:after"}, "run", "-f", workload, "--to", "a", "--yes")
	if code != exitFault {
		t.Fatalf("exit %d, want the fault exit %d", code, exitFault)
	}
	sf := latestRun(t, "a")
	if got := stepState(sf, "promote-a"); got != "Running" {
		t.Fatalf("promote-a is %q in the ledger, want Running", got)
	}
	tackOK(t, "resume", runName(sf), "-f", workload, "--yes")
	assertPrimary(t, "a", "b")
	assertGitMatches(t, "a", "a", "b")
	if got := psql(t, "a", "select count(*) from marks"); got != "2" {
		t.Errorf("site a has %s marks, want 2", got)
	}
}

func TestGitChangedDuringRun(t *testing.T) {
	_, code := tack(t, []string{"TACK_FAULT=git-commit:before"}, "run", "-f", workload, "--to", "b", "--yes")
	if code != exitFault {
		t.Fatalf("exit %d, want %d", code, exitFault)
	}
	run := runName(latestRun(t, "b"))

	gitPull(t)
	if err := os.WriteFile(filepath.Join(gitDir, "NOTES.md"), []byte("A commit made during a run.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, gitDir, "add", "NOTES.md")
	gitCmd(t, gitDir, "commit", "-q", "-m", "docs: a change during the run")
	gitCmd(t, gitDir, "push", "-q", "origin", "HEAD:main")
	other := gitCmd(t, gitDir, "rev-parse", "HEAD")

	out, code := tack(t, nil, "resume", run, "-f", workload, "--yes")
	if code != exitStopped || !strings.Contains(out, "GitChanged") {
		t.Fatalf("resume exit %d, want %d with GitChanged", code, exitStopped)
	}
	tackOK(t, "resume", run, "-f", workload, "--yes", "--accept-git-head", other)
	assertPrimary(t, "b", "a")
	head := assertGitMatches(t, "b", "a", "b")
	if parent := gitCmd(t, gitDir, "rev-parse", head+"^"); parent != other {
		t.Errorf("the run's commit is on %s, want the accepted %s", parent, other)
	}
}

func TestSiteLossBreakGlassAndReturn(t *testing.T) {
	// Site b (the primary) and Git are lost.
	docker(t, "pause", node("b"))
	docker(t, "stop", gitContainer)
	t.Cleanup(func() {
		exec.Command("docker", "unpause", node("b")).Run()
		exec.Command("docker", "start", gitContainer).Run()
	})

	out, code := tack(t, nil, "run", "-f", workload, "--to", "a", "--from", "b", "--mode", "site-loss", "--yes")
	if code != exitWaiting || !strings.Contains(out, "GitUnreachable") {
		t.Fatalf("exit %d, want %d waiting for Git", code, exitWaiting)
	}
	eventually(t, 3*time.Minute, "site a promoted during break-glass", func() string {
		if ip := jsonpath(t, "a", "cluster", "pg-a", "{.status.instancesReportedState.*.isPrimary}"); !strings.Contains(ip, "true") {
			return "isPrimary " + ip
		}
		return ""
	})
	psql(t, "a", "insert into marks values ('during-site-loss')")
	run := runName(latestRun(t, "a"))

	// Site b comes back, still primary: Git, which Flux at b reads, is down.
	docker(t, "unpause", node("b"))
	eventually(t, 2*time.Minute, "site b answers", func() string {
		if _, err := kubectlE("b", "get", "--raw", "/readyz"); err != nil {
			return err.Error()
		}
		return ""
	})
	out, code = tack(t, nil, "resume", run, "-f", workload, "--yes")
	if code != exitStopped || !strings.Contains(out, "FencedSiteIsPrimary") {
		t.Fatalf("resume exit %d, want %d with FencedSiteIsPrimary", code, exitStopped)
	}
	if got := jsonpath(t, "b", "cluster", "pg-b", "{.spec.replica.primary}"); got != "pg-b" {
		t.Fatalf("site b was changed before the operator approved: %s", got)
	}

	_, code = tack(t, nil, "resume", run, "-f", workload, "--yes", "--fence", "b")
	if code != exitWaiting {
		t.Fatalf("resume --fence exit %d, want %d (Git is still down)", code, exitWaiting)
	}
	if got := jsonpath(t, "b", "cluster", "pg-b", "{.spec.replica.primary}"); got != "pg-a" {
		t.Fatalf("site b not fenced: declares %s", got)
	}

	eventually(t, time.Minute, "the Git server starts", func() string {
		if out, err := exec.Command("docker", "start", gitContainer).CombinedOutput(); err != nil {
			return fmt.Sprintf("%v: %s", err, out)
		}
		return ""
	})
	eventually(t, time.Minute, "Git answers", func() string {
		url, _ := gitURL()
		if err := exec.Command("git", "ls-remote", url).Run(); err != nil {
			return err.Error()
		}
		return ""
	})
	tackOK(t, "resume", run, "-f", workload, "--yes")
	assertPrimary(t, "a", "b")
	assertGitMatches(t, "a", "a", "b")
	sf := latestRun(t, "a")
	if state, _ := runStatus(sf); state != "Succeeded" {
		t.Fatalf("run state %s", state)
	}
	out = tackOK(t, "status", run, "-f", workload)
	if !strings.Contains(out, "Rebuild") {
		t.Errorf("status lacks the Rebuild follow-up")
	}
}

func TestRebuildAndSwitchBack(t *testing.T) {
	// The runbook docs/runbooks/rebuild-old-primary.md, then a planned
	// switchover back to the rebuilt site. Before the rebuild, Tack
	// refuses it.
	out, code := tack(t, nil, "plan", "-f", workload, "--to", "b")
	if code == 0 || !strings.Contains(out, "rebuild it before a planned switchover") {
		t.Fatalf("plan to the failed-over site: exit %d, want a refusal", code)
	}
	gitPull(t)
	path := filepath.Join(gitDir, "clusters", "b", "cluster.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "pg_basebackup:") {
		t.Fatalf("clusters/b/cluster.yaml does not bootstrap from the other site:\n%s", s)
	}
	kubectl(t, "b", "-n", ns, "delete", "cluster", "pg-b", "--wait=true")
	kubectl(t, "b", "-n", "flux-system", "annotate", "--overwrite", "kustomization", "site-b", "reconcile.fluxcd.io/requestedAt="+time.Now().Format(time.RFC3339Nano))
	eventually(t, 15*time.Minute, "site b rebuilt as a healthy replica", func() string {
		p, err := kubectlE("b", "-n", ns, "get", "cluster", "pg-b", "-o", "jsonpath={.status.phase}")
		if err != nil || strings.TrimSpace(p) != "Cluster in healthy state" {
			return fmt.Sprintf("phase %q %v", p, err)
		}
		return ""
	})
	tackOK(t, "run", "-f", workload, "--to", "b", "--yes")
	assertPrimary(t, "b", "a")
	assertGitMatches(t, "b", "a", "b")
	if got := psql(t, "b", "select id from marks where id = 'during-site-loss'"); got != "during-site-loss" {
		t.Errorf("site b lacks a write made at site a during the site loss: %q", got)
	}
}

// TestRoutingFromStrait runs a switch with the routing check on, against the
// Strait that up.sh installs at both sites. Strait routes to pg-b before the
// run; the run succeeds only once Strait routes to pg-a at both sites.
func TestRoutingFromStrait(t *testing.T) {
	for _, s := range []string{"a", "b"} {
		kubectl(t, s, "apply", "-f", filepath.Join(repo, "test/e2e/fixtures/strait/siteservice.yaml"))
	}
	for _, s := range []string{"a", "b"} {
		eventually(t, 3*time.Minute, "Strait at site "+s+" routes to pg-b", func() string { return routeTo(s, "pg-b") })
	}
	b, err := os.ReadFile(workload)
	if err != nil {
		t.Fatal(err)
	}
	routed := filepath.Join(work, "workload-strait.yaml")
	w := strings.Replace(string(b), "  timeouts:", `  routing:
    strait:
      services:
      - {site: a, namespace: db, name: postgres}
      - {site: b, namespace: db, name: postgres}
      members:
      - {site: a, member: pg-a}
      - {site: b, member: pg-b}
  timeouts:`, 1)
	w = strings.Replace(w, "flux: 4m}", "flux: 4m, routing: 2m}", 1)
	if err := os.WriteFile(routed, []byte(w), 0o644); err != nil {
		t.Fatal(err)
	}

	tackOK(t, "run", "-f", routed, "--to", "a", "--yes")
	assertPrimary(t, "a", "b")
	sf := latestRun(t, "a")
	for _, id := range []string{"routing-a", "routing-b"} {
		if got := stepState(sf, id); got != "Succeeded" {
			t.Errorf("%s is %s", id, got)
		}
	}
	for _, s := range []string{"a", "b"} {
		if why := routeTo(s, "pg-a"); why != "" {
			t.Errorf("Strait at site %s: %s", s, why)
		}
	}
}

// routeTo returns "" when the site's SiteService is current and Strait routes
// it to the member only, with reason Active; else what it shows.
func routeTo(site, member string) string {
	out, err := kubectlE(site, "-n", ns, "get", "siteservice", "postgres", "-o", "json")
	if err != nil {
		return fmt.Sprintf("%v: %s", err, out)
	}
	var ss struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Status struct {
			ObservedGeneration int64 `json:"observedGeneration"`
			Route              struct {
				Members []string `json:"members"`
				Reason  string   `json:"reason"`
				Message string   `json:"message"`
			} `json:"route"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &ss); err != nil {
		return err.Error()
	}
	r := ss.Status.Route
	if ss.Status.ObservedGeneration == ss.Metadata.Generation && len(r.Members) == 1 && r.Members[0] == member && r.Reason == "Active" {
		return ""
	}
	return fmt.Sprintf("generation %d, observed %d, route %v, reason %q: %s",
		ss.Metadata.Generation, ss.Status.ObservedGeneration, r.Members, r.Reason, r.Message)
}
