//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// workload3 is the Workload of the three-site scenarios, written by setup
// when the environment has site c (TACK_E2E_SITES="a b c").
var workload3 = filepath.Join(work, "workload-3.yaml")

// Site c has a Cluster and no Pooler.
const workload3Template = `apiVersion: failover.tako.id/v1
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
  - name: c
    kubeContext: kind-tack-e2e-c
    cnpg:
      cluster: {namespace: db, name: pg-c}
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
      - {site: c, path: clusters/c}
  timeouts: {demotion: 6m, promotion: 8m, flux: 4m}
`

// assertFollows checks that a site declares primary, holds nothing and keeps
// no token, live and in Git, and that Flux there applied Git's head.
func assertFollows(t *testing.T, site, primary string) {
	t.Helper()
	if got := jsonpath(t, site, "cluster", clusterName(site), "{.spec.replica.primary}"); got != primary {
		t.Errorf("site %s declares %q, want %q", site, got, primary)
	}
	if got := jsonpath(t, site, "cluster", clusterName(site), "{.spec.replica.promotionToken}"); got != "" {
		t.Errorf("site %s keeps a promotion token", site)
	}
	if ann := jsonpath(t, site, "cluster", clusterName(site), "{.metadata.annotations}"); strings.Contains(ann, "failover.tako.id/held-by") {
		t.Errorf("site %s: Cluster %s is still held: %s", site, clusterName(site), ann)
	}
	head := gitPull(t)
	b, err := os.ReadFile(filepath.Join(gitDir, "clusters", site, "cluster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "primary: "+primary) {
		t.Errorf("Git at clusters/%s/cluster.yaml does not declare %s", site, primary)
	}
	eventually(t, 3*time.Minute, "Flux at site "+site+" applies Git's head", func() string {
		rev := strings.TrimSpace(kubectl(t, site, "-n", "flux-system", "get", "kustomization", "site-"+site, "-o", "jsonpath={.status.lastAppliedRevision}"))
		if !strings.HasSuffix(rev, head) {
			return fmt.Sprintf("applied %s, head %s", rev, head)
		}
		return ""
	})
}

// replicated waits until a row written at a primary reaches site.
func replicated(t *testing.T, site, mark string) {
	t.Helper()
	eventually(t, 3*time.Minute, "the mark "+mark+" reaches site "+site, func() string {
		if got := psql(t, site, "select id from marks where id = '"+mark+"'"); got != mark {
			return fmt.Sprintf("read %q", got)
		}
		return ""
	})
}

// siteLines returns the progress lines that name site c, for the record of
// how long its checks take.
func siteLines(out, site string) string {
	var l []string
	for _, line := range strings.Split(out, "\n") {
		if regexp.MustCompile(`^`+stamp).MatchString(line) && strings.Contains(line, "site "+site) {
			l = append(l, line)
		}
	}
	return strings.Join(l, "\n")
}

// #66: no three-site scenario had run on real API servers; #27 and #30 were
// tested on fakes. Three kind clusters, one CloudNativePG Cluster each, run
// in order: a planned switchover with c following; a planned switchover
// with c down, then a resume once c is back; and #27, a run taken over at a
// site that was lost, which must never resume. Run them alone:
// TACK_E2E_SITES="a b c" test/e2e/run.sh -run TestThreeSites.
func TestThreeSites(t *testing.T) {
	if !slices.Contains(sites, "c") {
		t.Skip(`the environment has no site c: TACK_E2E_SITES="a b c"`)
	}
	psql(t, "a", "create table if not exists marks (id text primary key)")
	psql(t, "a", "insert into marks values ('three-start') on conflict do nothing")
	replicated(t, "c", "three-start")

	t.Run("planned switchover, c follows", func(t *testing.T) {
		out := tackOK(t, "run", "-f", workload3, "--to", "b", "--yes")
		run := createdRun(t, out)
		assertPrimary(t, "b", "a")
		assertFollows(t, "c", "pg-b")
		psql(t, "b", "insert into marks values ('three-planned')")
		replicated(t, "c", "three-planned")
		if state, _ := runStatus(latestRun(t, "b")); state != "Succeeded" {
			t.Errorf("run %s: %s", run, state)
		}
	})

	t.Run("planned switchover, c down, then resumed", func(t *testing.T) {
		docker(t, "pause", node("c"))
		t.Cleanup(func() { dockerQuiet("unpause", node("c")) })
		start := time.Now()
		out, code := tack(t, nil, "run", "-f", workload3, "--to", "a", "--yes")
		took := time.Since(start)
		if code != 0 {
			t.Fatalf("run with c down: exit %d, want 0 with c's steps deferred", code)
		}
		run := createdRun(t, out)
		// #58 row 4: the record of what site c's checks cost a planned run.
		t.Logf("planned run with site c down took %s; its lines of site c:\n%s", took.Round(time.Second), siteLines(out, "c"))
		sf := latestRun(t, "a")
		if state, _ := runStatus(sf); state != "Succeeded" || !strings.Contains(fmt.Sprint(sf["status"]), "Deferred") {
			t.Fatalf("run %s: %s, want Succeeded with c's steps Deferred", run, state)
		}
		assertPrimary(t, "a", "b")
		docker(t, "unpause", node("c"))
		eventually(t, 2*time.Minute, "site c answers", func() string {
			if _, err := kubectlE("c", "get", "--raw", "/readyz"); err != nil {
				return err.Error()
			}
			return ""
		})
		tackOK(t, "resume", run, "-f", workload3, "--yes")
		if strings.Contains(fmt.Sprint(latestRun(t, "a")["status"]), "Deferred") {
			t.Errorf("run %s keeps a Deferred step after the resume", run)
		}
		assertFollows(t, "c", "pg-a")
		psql(t, "a", "insert into marks values ('three-resumed')")
		replicated(t, "c", "three-resumed")
	})

	t.Run("a run taken over at a lost site never resumes (#27)", func(t *testing.T) {
		// a, the primary, is lost. run-1 fails over to b, and its process
		// dies before the promotion.
		docker(t, "pause", node("a"))
		t.Cleanup(func() { dockerQuiet("unpause", node("a")); dockerQuiet("unpause", node("b")) })
		out, code := tack(t, []string{"TACK_FAULT=promote-b:before"}, "run", "-f", workload3, "--to", "b", "--from", "a", "--mode", "site-loss", "--yes")
		if code != exitFault {
			t.Fatalf("run-1: exit %d, want the fault exit %d", code, exitFault)
		}
		run1 := createdRun(t, out)

		// b is lost too, with run-1's ledger. run-2 takes run-1 over and
		// fails over to c.
		docker(t, "pause", node("b"))
		out, code = tack(t, nil, "run", "-f", workload3, "--to", "c", "--from", "a", "--mode", "site-loss", "--take-over", run1, "--yes")
		if code != 0 {
			t.Fatalf("run-2: exit %d, want 0 with b's steps deferred", code)
		}
		run2 := createdRun(t, out)
		eventually(t, 5*time.Minute, "site c promoted", func() string {
			if ip := jsonpath(t, "c", "cluster", "pg-c", "{.status.instancesReportedState.*.isPrimary}"); !strings.Contains(ip, "true") {
				return "isPrimary " + ip
			}
			return ""
		})
		psql(t, "c", "insert into marks values ('three-take-over')")

		// b comes back. run-1 must never resume: b would be a second
		// primary. Its ledger has no mark, since b gave no answer to run-2.
		docker(t, "unpause", node("b"))
		eventually(t, 2*time.Minute, "site b answers", func() string {
			if _, err := kubectlE("b", "get", "--raw", "/readyz"); err != nil {
				return err.Error()
			}
			return ""
		})
		out, code = tack(t, nil, "resume", run1, "-f", workload3, "--yes")
		if code != 1 || !strings.Contains(out, "was taken over by run "+run2) {
			t.Fatalf("resume of run-1: exit %d, want 1 naming run %s", code, run2)
		}
		if got := jsonpath(t, "b", "cluster", "pg-b", "{.spec.replica.primary}"); got == "pg-b" {
			t.Fatal("site b was promoted by run-1")
		}
		if ip := jsonpath(t, "b", "cluster", "pg-b", "{.status.instancesReportedState.*.isPrimary}"); strings.Contains(ip, "true") {
			t.Fatalf("site b runs a primary: %s", ip)
		}
		if got := strings.TrimSpace(kubectl(t, "b", "-n", ledgerNS, "get", "sfo", run1, "-o", "jsonpath={.status.supersededBy}")); got != "" {
			t.Errorf("run-1's ledger names %q as its taker; b gave no answer to run-2", got)
		}
		out, _ = tack(t, nil, "status", "-f", workload3)
		if !regexp.MustCompile(`(?m)^b\s+` + regexp.QuoteMeta(run1) + `\s.*\sTakenOver\s`).MatchString(out) {
			t.Errorf("tack status does not list %s as TakenOver:\n%s", run1, out)
		}

		// run-2's resume runs b's deferred steps over run-1's holds, and
		// fences b behind c.
		tackOK(t, "resume", run2, "-f", workload3, "--yes")
		if got := jsonpath(t, "b", "cluster", "pg-b", "{.spec.replica.primary}"); got != "pg-c" {
			t.Errorf("site b declares %q, want pg-c", got)
		}
		for kind, name := range map[string]string{"cluster": "pg-b", "pooler": poolerName("b")} {
			if ann := jsonpath(t, "b", kind, name, "{.metadata.annotations}"); strings.Contains(ann, run1) {
				t.Errorf("site b: %s %s still names run-1: %s", kind, name, ann)
			}
		}
	})
}

// dockerQuiet runs docker in a cleanup, where a failure must not stop the
// others.
func dockerQuiet(args ...string) { _ = exec.Command("docker", args...).Run() }
