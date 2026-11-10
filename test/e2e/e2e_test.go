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
	"regexp"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/takodotid/tack/internal/version"
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
	// operator is the ServiceAccount bound to the chart's roles only (rbacValues).
	operator = "tack-e2e-operator"
)

// rbacValues turn on the chart's roles for the objects of the e2e's Workload and its SiteService, and bind them to
// the ServiceAccount operator ([RFC#0009] Requirement 29). No GitOps object of the e2e has spec.postBuild, so they
// name no ConfigMap or Secret (row 12).
const rbacValues = `rbac:
  create: true
  prune: true
  subjects:
  - {kind: ServiceAccount, name: ` + operator + `, namespace: ` + ledgerNS + `}
  objects:
  - namespace: db
    clusters: [pg-a, pg-b]
    poolers: [pg-a-pooler-rw, pg-b-pooler-rw]
    siteServices: [postgres]
  - namespace: flux-system
    kustomizations: [site-a, site-b]
    gitRepositories: [tack-e2e]
`

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

// setup builds tack with the fault hook, installs the chart (the CRD and the
// roles of rbacValues) in the ledger namespace at both sites, creates the
// ServiceAccount operator there, and writes the Workload.
func setup() error {
	build := exec.Command("go", "build", "-tags", "e2efault", "-o", tackBin, "./cmd/tack")
	build.Dir = repo
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("build tack: %w", err)
	}
	values := filepath.Join(work, "chart-values.yaml")
	if err := os.WriteFile(values, []byte(rbacValues), 0o644); err != nil {
		return err
	}
	for _, s := range []string{"a", "b"} {
		helm := exec.Command("helm", "--kubeconfig", kubeconfig, "--kube-context", context(s),
			"upgrade", "--install", "tack", filepath.Join(repo, "deploy/helm/tack"),
			"--namespace", ledgerNS, "--create-namespace", "--wait", "-f", values)
		if out, err := helm.CombinedOutput(); err != nil {
			return fmt.Errorf("site %s: install chart: %v: %s", s, err, out)
		}
		if out, err := kubectlIn(s, "apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: "+operator+", namespace: "+ledgerNS+"}\n", "apply", "-f", "-"); err != nil {
			return fmt.Errorf("site %s: create ServiceAccount %s: %v: %s", s, operator, err, out)
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
const workloadTemplate = `apiVersion: failover.tako.id/v1
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

// stamp matches the time that starts each progress line ([RFC#0009]
// Requirement 27).
const stamp = `[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z `

// createdRun returns the name of the run whose ledger tack created, from
// tack's output.
func createdRun(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + stamp + `ledger ` + ledgerNS + `/(\S+) created$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("tack created no ledger:\n%s", out)
	}
	return m[1]
}

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
	out = tackOK(t, "run", "-f", workload, "--to", "b", "--yes")
	run := createdRun(t, out)
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
	assertRecorded(t, run, out)
}

// assertRecorded checks, against a real API server, what a planned run
// that succeeded in one process records: each progress line's time, the
// ledger's history with the user the API server names, the tack that
// wrote it, its Events, the field manager tack, and the columns Reason and
// Abandoned
// ([RFC#0003] Requirements 22 to 25, [RFC#0009] Requirement 27).
func assertRecorded(t *testing.T, run, out string) {
	t.Helper()
	if !regexp.MustCompile(`(?m)^` + stamp + `  start     demote-a: `).MatchString(out) {
		t.Errorf("no progress line of demote-a with its time")
	}
	get := func(path string) string {
		return strings.TrimSpace(kubectl(t, "b", "-n", ledgerNS, "get", "sitefailover", run, "-o", "jsonpath="+path))
	}
	if got := get(`{range .status.history[*]}{.state}/{.user}/{.version}/{.flags[*]};{end}`); got != "Running/kubernetes-admin/"+version.Version+"/--yes;Succeeded/kubernetes-admin/"+version.Version+"/--yes;" {
		t.Errorf("history %q, want Running and Succeeded by kubernetes-admin with tack %s and --yes", got, version.Version)
	}
	if got := get(`{.status.writtenBy}`); got != version.Version {
		t.Errorf("writtenBy %q, want %q", got, version.Version)
	}
	if got := get(`{.metadata.managedFields[*].manager}`); strings.Trim(strings.ReplaceAll(got, "tack", ""), " ") != "" {
		t.Errorf("field managers %q, want tack only", got)
	}
	events := kubectl(t, "b", "-n", ledgerNS, "get", "events", "--field-selector", "involvedObject.kind=SiteFailover,involvedObject.name="+run,
		"-o", `jsonpath={range .items[*]}{.reason}/{.type}/{.source.component}{"\n"}{end}`)
	for _, want := range []string{"RunRunning/Normal/tack", "StepStarted/Normal/tack", "StepSucceeded/Normal/tack", "RunSucceeded/Normal/tack"} {
		if !strings.Contains(events, want+"\n") {
			t.Errorf("no Event %s on the ledger:\n%s", want, events)
		}
	}
	if head := strings.SplitN(kubectl(t, "b", "-n", ledgerNS, "get", "sfo", run), "\n", 2)[0]; !regexp.MustCompile(`STATE\s+REASON\s+ABANDONED\s+PHASE\s+AGE`).MatchString(head) {
		t.Errorf("kubectl get sfo prints %q, want the columns STATE, REASON, ABANDONED, PHASE, AGE ([RFC#0003] Requirement 15)", head)
	}
	status := tackOK(t, "status", run, "-f", workload)
	if !strings.Contains(status, "History:\n") || !strings.Contains(status, "Written by: tack "+version.Version) || !strings.Contains(status, ", attempts 1\n") {
		t.Errorf("tack status lacks the history, the writer or the attempts")
	}
}

// A site whose check fails with a refused credential, or whose context is
// missing from the kubeconfig, is not a lost site. A site-loss plan away
// from the primary at b is refused, not accepted with b read as lost.
// Issue #25.
func TestSiteLossPlanRefusesSiteThatCannotBeChecked(t *testing.T) {
	src, err := os.ReadFile(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	refused := filepath.Join(work, "kubeconfig-refused-b")
	if err := os.WriteFile(refused, src, 0o600); err != nil {
		t.Fatal(err)
	}
	user := "users." + context("b")
	for _, args := range [][]string{
		{"config", "unset", user + ".client-certificate-data"},
		{"config", "unset", user + ".client-key-data"},
		{"config", "set-credentials", context("b"), "--token=not-a-real-token"},
	} {
		if out, err := exec.Command("kubectl", append([]string{"--kubeconfig", refused}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("kubectl %v: %v\n%s", args, err, out)
		}
	}
	w, err := os.ReadFile(workload)
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(work, "workload-missing-context.yaml")
	if err := os.WriteFile(missing, []byte(strings.Replace(string(w), "kubeContext: "+context("b"), "kubeContext: tack-e2e-missing", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"refused credential": {"plan", "--kubeconfig", refused, "-f", workload, "--to", "a", "--from", "b", "--mode", "site-loss"},
		"missing context":    {"plan", "-f", missing, "--to", "a", "--from", "b", "--mode", "site-loss"},
	} {
		out, code := tack(t, nil, args...)
		if code != 1 || !strings.Contains(out, "site b") || strings.Contains(out, "Nothing was changed") {
			t.Errorf("%s: plan exit %d, want 1 with an error naming site b", name, code)
		}
	}
}

// A short cut of the to site's API: the plan checks the site again and
// succeeds. Pausing the site's node stands in for a network cut between
// the operator's host and the site: its API does not answer for 10 s.
func TestPlanDuringBlip(t *testing.T) {
	docker(t, "pause", node("a"))
	unpaused := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Second)
		exec.Command("docker", "unpause", node("a")).Run()
		close(unpaused)
	}()
	t.Cleanup(func() { <-unpaused })

	out, code := tack(t, nil, "plan", "-f", workload, "--to", "a")
	<-unpaused
	if code != 0 {
		t.Fatalf("plan exit %d, want 0 after a retry", code)
	}
	for _, want := range []string{"retry     site a did not answer check 1 of 3", "\n  site a answered on check ", "Nothing was changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q", want)
		}
	}
	eventually(t, 3*time.Minute, "site a is healthy again", func() string {
		if p := jsonpath(t, "a", "cluster", clusterName("a"), "{.status.phase}"); p != "Cluster in healthy state" {
			return "phase " + p
		}
		return ""
	})
}

// A run dies as it starts promote-a; the operator promotes b again by hand
// (docs/runbooks/no-primary-after-demotion.md, option 2), and leaves the
// holds. A resume stops before promote-a while b is primary (issue #45).
// The next run takes the stale run over, dies right after promoting site
// a, and resumes from its ledger.
func TestInterruptedRunResumes(t *testing.T) {
	_, code := tack(t, []string{"TACK_FAULT=promote-a:before"}, "run", "-f", workload, "--to", "a", "--yes")
	if code != exitFault {
		t.Fatalf("exit %d, want the fault exit %d", code, exitFault)
	}
	stale := runName(latestRun(t, "a"))
	kubectl(t, "b", "-n", ns, "patch", "cluster", clusterName("b"), "--type", "merge", "-p", `{"spec":{"replica":{"primary":"pg-b"}}}`)
	eventually(t, 5*time.Minute, "site b is a healthy primary again", func() string {
		p := jsonpath(t, "b", "cluster", clusterName("b"), "{.status.phase}")
		ip := jsonpath(t, "b", "cluster", clusterName("b"), "{.status.instancesReportedState.*.isPrimary}")
		if p != "Cluster in healthy state" || !strings.Contains(ip, "true") {
			return fmt.Sprintf("phase %q, isPrimary %q", p, ip)
		}
		return ""
	})
	kubectl(t, "b", "-n", ns, "patch", "pooler", poolerName("b"), "--type", "merge", "-p", `{"spec":{"instances":1}}`)
	before := jsonpath(t, "a", "cluster", clusterName("a"), "{.metadata.generation}")
	out, code := tack(t, nil, "resume", stale, "-f", workload, "--yes")
	if code != exitStopped || !strings.Contains(out, "FencedSiteIsPrimary") || !strings.Contains(out, "promote-a: site b answers as primary") {
		t.Fatalf("resume of %s: exit %d, want %d with FencedSiteIsPrimary before promote-a", stale, code, exitStopped)
	}
	if after := jsonpath(t, "a", "cluster", clusterName("a"), "{.metadata.generation}"); after != before {
		t.Errorf("Cluster pg-a changed: generation %s, then %s", before, after)
	}
	if ip := jsonpath(t, "a", "cluster", clusterName("a"), "{.status.instancesReportedState.*.isPrimary}"); strings.Contains(ip, "true") {
		t.Fatal("site a was promoted while site b is primary")
	}
	if got := stepState(latestRun(t, "a"), "promote-a"); got != "Running" {
		t.Errorf("promote-a is %q, want Running as the process left it", got)
	}

	// The process dies right after promoting site a, before the ledger
	// records it.
	_, code = tack(t, []string{"TACK_FAULT=promote-a:after"}, "run", "-f", workload, "--to", "a", "--yes", "--take-over", stale)
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
	if out, code := tack(t, nil, "resume", stale, "-f", workload, "--yes"); code != 1 || !strings.Contains(out, "was taken over by run "+runName(sf)) {
		t.Errorf("resume of the stale run %s: exit %d, want 1, taken over by %s", stale, code, runName(sf))
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
	// Issue #29: the process dies once the run's commit is pushed, and a
	// commit lands on top of it before Flux applies it.
	_, code = tack(t, []string{"TACK_FAULT=reconcile-git-a:before"}, "resume", run, "-f", workload, "--yes", "--accept-git-head", other)
	if code != exitFault {
		t.Fatalf("resume exit %d, want the fault exit %d", code, exitFault)
	}
	commit := gitPull(t)
	if err := os.WriteFile(filepath.Join(gitDir, "NOTES.md"), []byte("A commit made after the run's commit.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, gitDir, "commit", "-q", "-am", "docs: a change after the run's commit")
	gitCmd(t, gitDir, "push", "-q", "origin", "HEAD:main")
	later := gitCmd(t, gitDir, "rev-parse", "HEAD")
	out, code = tack(t, nil, "resume", run, "-f", workload, "--yes")
	if code != exitStopped || !strings.Contains(out, "GitChanged") || !strings.Contains(out, "--accept-git-head "+later) {
		t.Fatalf("resume after a push on top of the run's commit: exit %d, want %d with GitChanged before the release", code, exitStopped)
	}
	if got := stepState(latestRun(t, "b"), "verify-git-a"); got != "Failed" {
		t.Errorf("verify-git-a is %q, want Failed", got)
	}
	tackOK(t, "resume", run, "-f", workload, "--yes", "--accept-git-head", later)
	assertPrimary(t, "b", "a")
	head := assertGitMatches(t, "b", "a", "b")
	if head != later {
		t.Errorf("Git head %s, want %s", head, later)
	}
	if parent := gitCmd(t, gitDir, "rev-parse", later+"^"); parent != commit {
		t.Errorf("the later commit is on %s, want the run's %s", parent, commit)
	}
	if parent := gitCmd(t, gitDir, "rev-parse", commit+"^"); parent != other {
		t.Errorf("the run's commit is on %s, want the accepted %s", parent, other)
	}
	if got := stepState(latestRun(t, "b"), "verify-noop-a"); got != "Succeeded" {
		t.Errorf("verify-noop-a is %q", got)
	}
	// #35 row 29: each stop stays in the history, with the head each
	// resume accepted. The processes that the faults ended recorded no end.
	history := strings.TrimSpace(kubectl(t, "b", "-n", ledgerNS, "get", "sitefailover", run, "-o", `jsonpath={range .status.history[*]}{.state}/{.reason}/{.flags[*]};{end}`))
	want := "Running//--yes;Running//--yes;Stopped/GitChanged/--yes;Running//--accept-git-head=" + other + " --yes;Running//--yes;Stopped/GitChanged/--yes;Running//--accept-git-head=" + later + " --yes;Succeeded//--accept-git-head=" + later + " --yes;"
	if history != want {
		t.Errorf("history\n%s\nwant\n%s", history, want)
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
	if !strings.Contains(out, "If site b still runs, it stays primary") || !strings.Contains(out, version.DocLink("docs/runbooks/site-loss.md")) {
		t.Error("the site-loss plan lacks the note on a site cut off from this host (issue #26)")
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
	assertUnchanged := func() {
		t.Helper()
		if got := jsonpath(t, "b", "cluster", "pg-b", "{.spec.replica.primary}"); got != "pg-b" {
			t.Fatalf("site b was changed before the operator approved: %s", got)
		}
		for kind, name := range map[string]string{"cluster": "pg-b", "pooler": poolerName("b")} {
			ann := jsonpath(t, "b", kind, name, "{.metadata.annotations}")
			if strings.Contains(ann, "kustomize.toolkit.fluxcd.io/reconcile") || strings.Contains(ann, "failover.tako.id/held-by") {
				t.Fatalf("site b: %s %s was held before the operator approved: %s", kind, name, ann)
			}
		}
		if got := stepState(latestRun(t, "a"), "hold-b-cluster-pg-b"); got != "Deferred" {
			t.Fatalf("hold-b-cluster-pg-b is %q, want Deferred", got)
		}
	}
	assertUnchanged()

	// Issue #23: a second resume without --fence stops again, and still
	// changes nothing at site b.
	out, code = tack(t, nil, "resume", run, "-f", workload, "--yes")
	if code != exitStopped || !strings.Contains(out, "FencedSiteIsPrimary") {
		t.Fatalf("second resume exit %d, want %d with FencedSiteIsPrimary", code, exitStopped)
	}
	assertUnchanged()

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

// operatorKubeconfig writes a kubeconfig whose context for each site has the
// admin's cluster and a token of the ServiceAccount operator, and returns its
// path. The token is never printed.
func operatorKubeconfig(t *testing.T) string {
	t.Helper()
	cfg, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"a", "b"} {
		token, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "--context", context(s),
			"-n", ledgerNS, "create", "token", operator, "--duration", "1h").Output()
		if err != nil {
			t.Fatalf("site %s: create a token of ServiceAccount %s: %v", s, operator, err)
		}
		user := operator + "-" + s
		cfg.AuthInfos[user] = &clientcmdapi.AuthInfo{Token: strings.TrimSpace(string(token))}
		cfg.Contexts[context(s)].AuthInfo = user
	}
	path := filepath.Join(work, "kubeconfig-"+operator)
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRoutingFromStrait runs a switch with the routing check on, against the
// Strait that up.sh installs at both sites. Strait routes to pg-b before the
// run; the run succeeds only once Strait routes to pg-a at both sites.
//
// tack plans, runs and reads the run as the ServiceAccount operator, bound to
// the chart's roles only, with Kubernetes' default bindings: the roles grant
// every permission of the run, the routing check (row 13 of the permissions
// table), the ledger's Events (row 14) and the history's user (row 15)
// among them ([RFC#0009] Requirement 29).
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

	restricted := operatorKubeconfig(t)
	tackOK(t, "plan", "--kubeconfig", restricted, "-f", routed, "--to", "a")
	run := createdRun(t, tackOK(t, "run", "--kubeconfig", restricted, "-f", routed, "--to", "a", "--yes"))
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

	user := "system:serviceaccount:" + ledgerNS + ":" + operator
	if got := strings.TrimSpace(kubectl(t, "a", "-n", ledgerNS, "get", "sitefailover", run, "-o", `jsonpath={range .status.history[*]}{.state}/{.user};{end}`)); got != "Running/"+user+";Succeeded/"+user+";" {
		t.Errorf("history %q, want Running and Succeeded by %s", got, user)
	}
	events := kubectl(t, "a", "-n", ledgerNS, "get", "events", "--field-selector", "involvedObject.kind=SiteFailover,involvedObject.name="+run,
		"-o", `jsonpath={range .items[*]}{.reason}{"\n"}{end}`)
	for _, want := range []string{"RunRunning", "StepSucceeded", "RunSucceeded"} {
		if !strings.Contains(events, want+"\n") {
			t.Errorf("no Event %s on the ledger:\n%s", want, events)
		}
	}
	if status := tackOK(t, "status", "--kubeconfig", restricted, run, "-f", routed); !strings.Contains(status, "State: Succeeded") {
		t.Errorf("tack status as %s does not show the run Succeeded", operator)
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

// TestTakeOver: a run dies right after it created its ledger; it cannot
// resume (issue #28). A second run takes it over and dies at its first
// step. Resumed with a kubeconfig whose context for site a reaches cluster
// b, it stops before it acts there (issue #32). A third run takes it over
// and records it; the second run cannot resume (issue #27).
func TestTakeOver(t *testing.T) {
	_, code := tack(t, []string{"TACK_FAULT=create-ledger:after"}, "run", "-f", workload, "--to", "b", "--yes")
	if code != exitFault {
		t.Fatalf("exit %d, want the fault exit %d", code, exitFault)
	}
	never := latestRun(t, "b")
	if state, _ := runStatus(never); state != "" {
		t.Fatalf("the ledger has state %q; the API server should have dropped its status on create", state)
	}
	out, code := tack(t, nil, "resume", runName(never), "-f", workload, "--yes")
	if code != 1 || !strings.Contains(out, "never recorded its start") {
		t.Fatalf("resume of a ledger without a start: exit %d, want 1 with a refusal", code)
	}

	// When this run is planned in the same second as the run it takes
	// over, it gets that run's name with -2 appended (issue #37). Two
	// ledgers can then share a creation second, so the names come from
	// tack's output.
	out, code = tack(t, []string{"TACK_FAULT=hold-a-cluster-pg-a:before"}, "run", "-f", workload, "--to", "b", "--take-over", runName(never), "--yes")
	if code != exitFault {
		t.Fatalf("exit %d, want the fault exit %d", code, exitFault)
	}
	old := createdRun(t, out)
	out, code = tack(t, nil, "run", "-f", workload, "--to", "b", "--yes")
	if code != 1 || !strings.Contains(out, "--take-over "+old) {
		t.Fatalf("a run without --take-over: exit %d, want 1 naming %s", code, old)
	}

	src, err := os.ReadFile(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	swapped := filepath.Join(work, "kubeconfig-a-is-b")
	if err := os.WriteFile(swapped, src, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("kubectl", "--kubeconfig", swapped, "config", "set-context", context("a"),
		"--cluster", context("b"), "--user", context("b")).CombinedOutput(); err != nil {
		t.Fatalf("kubectl config set-context: %v\n%s", err, out)
	}
	out, code = tack(t, nil, "resume", "--kubeconfig", swapped, old, "-f", workload, "--yes")
	if code != exitStopped || !strings.Contains(out, "SiteMismatch") {
		t.Fatalf("resume through a context that reaches cluster b: exit %d, want %d with SiteMismatch", code, exitStopped)
	}
	if ann := jsonpath(t, "a", "cluster", "pg-a", "{.metadata.annotations}"); strings.Contains(ann, "failover.tako.id/held-by") {
		t.Fatalf("site a was held: %s", ann)
	}

	run := createdRun(t, tackOK(t, "run", "-f", workload, "--to", "b", "--take-over", old, "--yes"))
	assertPrimary(t, "b", "a")
	assertGitMatches(t, "b", "a", "b")
	if got := strings.TrimSpace(kubectl(t, "b", "-n", ledgerNS, "get", "sitefailover", run, "-o", "jsonpath={.spec.takeOver}")); got != old {
		t.Errorf("%s: spec.takeOver %q, want %s", run, got, old)
	}
	if got := strings.TrimSpace(kubectl(t, "b", "-n", ledgerNS, "get", "sitefailover", old, "-o", "jsonpath={.status.supersededBy}")); got != run {
		t.Errorf("%s: status.supersededBy %q, want %s", old, got, run)
	}

	out, code = tack(t, nil, "resume", old, "-f", workload, "--yes")
	if code != 1 || !strings.Contains(out, "was taken over by run "+run) {
		t.Fatalf("resume of the old run: exit %d, want 1 with a refusal", code)
	}
	assertPrimary(t, "b", "a")
}

// #35 row 19 (RFC0005 Requirement 8). A commit pushed after the run's
// commit sets the Pooler's instances at site a, a field the run changed,
// to another value. Once the operator accepts that head, the server-side
// render and diff before release stops the run with GitDiffersFromLive,
// before anything at site a is released. With Git fixed and its head
// accepted, the run finishes. Site b is the primary before the run.
func TestGitDiffersFromLive(t *testing.T) {
	_, code := tack(t, []string{"TACK_FAULT=reconcile-git-a:before"}, "run", "-f", workload, "--to", "a", "--yes")
	if code != exitFault {
		t.Fatalf("exit %d, want the fault exit %d", code, exitFault)
	}
	run := runName(latestRun(t, "a"))
	gitPull(t)
	path := filepath.Join(gitDir, "clusters", "a", "pooler.yaml")
	setInstances := func(from, to, msg string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "\n  instances: "+from+"\n") {
			t.Fatalf("clusters/a/pooler.yaml does not set instances: %s:\n%s", from, b)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(b), "\n  instances: "+from+"\n", "\n  instances: "+to+"\n", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, gitDir, "commit", "-q", "-am", msg)
		gitCmd(t, gitDir, "push", "-q", "origin", "HEAD:main")
		return gitCmd(t, gitDir, "rev-parse", "HEAD")
	}
	wrong := setInstances("1", "2", "chore: two Pooler instances at site a")

	out, code := tack(t, nil, "resume", run, "-f", workload, "--yes", "--accept-git-head", wrong)
	if code != exitStopped || !strings.Contains(out, "GitDiffersFromLive") || !strings.Contains(out, "spec.instances") {
		t.Fatalf("resume exit %d, want %d with GitDiffersFromLive naming spec.instances", code, exitStopped)
	}
	sf := latestRun(t, "a")
	if state, reason := runStatus(sf); state != "Stopped" || reason != "GitDiffersFromLive" {
		t.Errorf("run %s (%s), want Stopped (GitDiffersFromLive)", state, reason)
	}
	if got := stepState(sf, "verify-git-a"); got != "Failed" {
		t.Errorf("verify-git-a is %q, want Failed", got)
	}
	for _, id := range []string{"release-a-cluster-pg-a", "release-a-pooler-pg-a-pooler-rw"} {
		if got := stepState(sf, id); got != "Pending" {
			t.Errorf("%s is %q, want Pending", id, got)
		}
	}
	if ann := jsonpath(t, "a", "pooler", poolerName("a"), "{.metadata.annotations}"); !strings.Contains(ann, "failover.tako.id/held-by") {
		t.Errorf("site a's Pooler is no longer held: %s", ann)
	}
	if got := jsonpath(t, "a", "pooler", poolerName("a"), "{.spec.instances}"); got != "1" {
		t.Errorf("site a's Pooler has %s instances, want 1: Flux applied Git", got)
	}

	fixed := setInstances("2", "1", "fix: one Pooler instance at site a")
	tackOK(t, "resume", run, "-f", workload, "--yes", "--accept-git-head", fixed)
	assertPrimary(t, "a", "b")
	if head := assertGitMatches(t, "a", "a", "b"); head != fixed {
		t.Errorf("Git head %s, want %s", head, fixed)
	}
}

// #59, observed in a two-site test of v1.3.0: with a site down, tack status
// left out its runs and exited 0, and kubectl get sfo showed an abandoned
// run as Stopped. Pausing a site's node stands in for a site that gives no
// answer. A run dies before its first step, with its ledger at b and no
// hold. With b down, tack status and tack abandon name b. With a down,
// tack status lists the run and names a, tack abandon marks the run, and
// kubectl get sfo at b shows the mark in its Abandoned column ([RFC#0003]
// Requirement 15). Site a is the primary before the run.
func TestUnreachableSiteDuringStatusAbandonAndGet(t *testing.T) {
	_, code := tack(t, []string{"TACK_FAULT=hold-a-cluster-pg-a:before"}, "run", "-f", workload, "--to", "b", "--yes")
	if code != exitFault {
		t.Fatalf("exit %d, want the fault exit %d", code, exitFault)
	}
	run := runName(latestRun(t, "b"))

	down := func(site string) (up func()) {
		t.Helper()
		docker(t, "pause", node(site))
		paused := true
		t.Cleanup(func() {
			if paused {
				exec.Command("docker", "unpause", node(site)).Run()
			}
		})
		return func() {
			t.Helper()
			docker(t, "unpause", node(site))
			paused = false
			eventually(t, 3*time.Minute, "site "+site+" is healthy again", func() string {
				if _, err := kubectlE(site, "get", "--raw", "/readyz"); err != nil {
					return err.Error()
				}
				if p := jsonpath(t, site, "cluster", clusterName(site), "{.status.phase}"); p != "Cluster in healthy state" {
					return "phase " + p
				}
				return ""
			})
		}
	}
	wants := func(what, out string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("%s lacks %q", what, w)
			}
		}
	}

	// The ledger's site gives no answer.
	up := down("b")
	out, code := tack(t, nil, "status", "-f", workload)
	if code != 1 {
		t.Errorf("status with b down: exit %d, want 1", code)
	}
	wants("status with b down", out, "Sites that gave no answer; their runs are not listed:\n  site b: ",
		"the list of runs is incomplete: site b gave no answer")
	out, code = tack(t, nil, "abandon", run, "-f", workload, "--yes")
	if code != 1 {
		t.Errorf("abandon with b down: exit %d, want 1", code)
	}
	wants("abandon with b down", out, fmt.Sprintf("run %q not found at any reachable site; site b gave no answer, so its runs were not read: ", run))
	up()
	if a := strings.TrimSpace(kubectl(t, "b", "-n", ledgerNS, "get", "sitefailover", run, "-o", "jsonpath={.status.abandoned}")); a != "" {
		t.Fatalf("run %s was abandoned while its site gave no answer: %s", run, a)
	}

	// Another site gives no answer.
	up = down("a")
	out, code = tack(t, nil, "status", "-f", workload)
	if code != 1 {
		t.Errorf("status with a down: exit %d, want 1", code)
	}
	wants("status with a down", out, run, "Sites that gave no answer; their runs are not listed:\n  site a: ",
		"the list of runs is incomplete: site a gave no answer")
	out, code = tack(t, nil, "abandon", run, "-f", workload, "--reason", "e2e: a site gives no answer", "--yes")
	if code != 0 {
		t.Fatalf("abandon with a down: exit %d, want 0", code)
	}
	wants("abandon with a down", out, "run "+run+" abandoned", "kubectl get sfo shows the mark in its Abandoned column")
	// The table, as an operator reads it: the column by its header, not
	// the field by its path.
	lines := strings.Split(strings.TrimSpace(kubectl(t, "b", "-n", ledgerNS, "get", "sfo", run)), "\n")
	col := strings.Index(lines[0], "ABANDONED")
	if len(lines) != 2 || col < 0 || len(lines[1]) <= col {
		t.Fatalf("kubectl get sfo %s has no ABANDONED column:\n%s", run, strings.Join(lines, "\n"))
	}
	if v := strings.Fields(lines[1][col:]); len(v) == 0 || v[0] == "<none>" || v[0] == "<unknown>" {
		t.Errorf("kubectl get sfo %s: ABANDONED %q, want the mark's age:\n%s", run, v, strings.Join(lines, "\n"))
	}
	out, code = tack(t, nil, "status", "-f", workload)
	if code != 1 || !regexp.MustCompile(`(?m)^b\s+`+regexp.QuoteMeta(run)+`\s.*\sAbandoned\s`).MatchString(out) {
		t.Errorf("status after the abandon, a down: exit %d, want 1 listing %s as Abandoned", code, run)
	}
	up()
}

// #58 row 1. Every run of the e2e is finished by now: tack prune deletes the
// ledgers of old runs that succeeded, through the API servers'
// preconditions, and keeps the newest and every abandoned or taken-over run
// ([RFC#0003] Requirement 27). It runs as the ServiceAccount operator,
// bound to the chart's roles with rbac.prune: row 16 is enough.
func TestPrune(t *testing.T) {
	abandoned := runName(latestRun(t, "b"))
	restricted := operatorKubeconfig(t)
	out, code := tack(t, nil, "prune", "--kubeconfig", restricted, "-f", workload, "--keep", "2", "--yes")
	if code != 0 {
		t.Fatalf("prune: exit %d, want 0", code)
	}
	deleted := regexp.MustCompile(`(?m)^`+stamp+`deleted   ledger `+ledgerNS+`/(\S+) at site (\S+)$`).FindAllStringSubmatch(out, -1)
	if len(deleted) == 0 {
		t.Fatal("prune deleted no ledger")
	}
	for _, d := range deleted {
		if _, err := kubectlE(d[2], "-n", ledgerNS, "get", "sfo", d[1]); err == nil {
			t.Errorf("run %s is still at site %s", d[1], d[2])
		}
	}
	for _, want := range []string{abandoned + " ", "keep: abandoned", "keep: taken over", "keep: one of the 2 newest"} {
		if !strings.Contains(out, want) {
			t.Errorf("prune lacks %q", want)
		}
	}
	if _, err := kubectlE("b", "-n", ledgerNS, "get", "sfo", abandoned); err != nil {
		t.Errorf("the abandoned run %s is gone: %v", abandoned, err)
	}
	out, code = tack(t, nil, "prune", "--kubeconfig", restricted, "-f", workload, "--keep", "2")
	if code != 0 || !strings.Contains(out, "Nothing to delete.") {
		t.Errorf("second prune: exit %d, want 0 with nothing to delete", code)
	}
}
