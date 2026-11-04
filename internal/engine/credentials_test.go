package engine_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/testenv"
)

// realSite checks one site through a real kubeconfig, and every other site
// through the fakes. The fakes still serve every site's objects.
type realSite struct {
	*testenv.Clients
	site string
	real kube.Clients
}

func (c realSite) Reachable(ctx context.Context, site string) error {
	if site == c.site {
		return c.real.Reachable(ctx, site)
	}
	return c.Clients.Reachable(ctx, site)
}

// kubeconfigFile writes a kubeconfig with context site-a for server. An
// empty server writes a kubeconfig without that context.
func kubeconfigFile(t *testing.T, server string) string {
	t.Helper()
	cfg := "apiVersion: v1\nkind: Config\n"
	if server != "" {
		cfg += fmt.Sprintf(`clusters:
- name: a
  cluster: {server: %q}
users:
- name: a
  user: {token: not-a-real-token}
contexts:
- name: site-a
  context: {cluster: a, user: a}
`, server)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// statusServer answers every request with an API status of code.
func statusServer(t *testing.T, code int, reason string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":%q,"reason":%q,"code":%d}`, reason, reason, code)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// siteAChecks are the ways the check of site a fails without a lost site:
// a context missing from the kubeconfig, a token the site refuses (401),
// and a token without the grant (403). Issue #25.
func siteAChecks(t *testing.T) map[string]kube.Clients {
	return map[string]kube.Clients{
		"missing context": kube.NewKubeconfig(kubeconfigFile(t, ""), map[string]string{"a": "site-a"}),
		"401":             kube.NewKubeconfig(kubeconfigFile(t, statusServer(t, http.StatusUnauthorized, "Unauthorized")), map[string]string{"a": "site-a"}),
		"403":             kube.NewKubeconfig(kubeconfigFile(t, statusServer(t, http.StatusForbidden, "Forbidden")), map[string]string{"a": "site-a"}),
	}
}

// A site-loss plan must not take a site whose check fails with a
// configuration or credential error for a lost site: it may be a live
// primary.
func TestSiteLossPlanRefusesSiteThatCannotBeChecked(t *testing.T) {
	for name, real := range siteAChecks(t) {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			env := f.env("b")
			env.Clients = realSite{Clients: f.clients, site: "a", real: real}
			_, err := env.MakePlan(context.Background(), engine.Request{
				Run: "run-1", Workload: f.w.Name, Spec: &f.w.Spec,
				Request: driver.Request{From: "a", To: "b", Mode: failoverv1.ModeSiteLoss},
			})
			if err == nil {
				t.Fatal("the site-loss plan was accepted: site a counted as lost")
			}
			if !strings.Contains(err.Error(), "site a") {
				t.Errorf("err %v does not name site a", err)
			}
		})
	}
}

// A lost site comes back, but its check fails with a configuration or
// credential error. The resume stops before its steps; they stay Deferred
// and nothing is held there.
func TestResumeStopsOnSiteThatCannotBeChecked(t *testing.T) {
	for name, real := range siteAChecks(t) {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.lostSiteReturnsAsPrimary(false)
			env := f.env("b")
			env.Clients = realSite{Clients: f.clients, site: "a", real: real}
			err := env.Resume(context.Background(), f.ledger("b"), engine.Options{})
			sf := f.ledger("b")
			if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonStepFailed {
				t.Fatalf("err %v, state %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
			}
			for _, id := range []string{"hold-a-cluster-pg-a", "fence-a"} {
				if st := stepState(sf, id); st != failoverv1.StepDeferred {
					t.Errorf("%s is %s, want Deferred", id, st)
				}
			}
			if n := holdsAt(f.flux, "a"); n != 0 {
				t.Errorf("%d holds at site a", n)
			}
		})
	}
}
