package kube

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// kubeconfig writes a kubeconfig whose context site-a points at server
// with user, a YAML flow mapping. insecure skips the check of the server's
// certificate.
func kubeconfig(t *testing.T, server, user string, insecure bool) string {
	t.Helper()
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: a
  cluster: {server: %q, insecure-skip-tls-verify: %v}
users:
- name: a
  user: %s
contexts:
- name: site-a
  context: {cluster: a, user: a}
`, server, insecure, user)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"code %d","code":%d}`, code, code)
	}
}

// Only a check that gets no answer counts toward a lost site. Issue #25.
func TestReachableKinds(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + closed.Addr().String()
	closed.Close()

	serve := func(h http.Handler) string {
		s := httptest.NewServer(h)
		t.Cleanup(s.Close)
		return s.URL
	}
	tlsServer := httptest.NewTLSServer(status(http.StatusOK))
	t.Cleanup(tlsServer.Close)

	token := "{token: not-a-real-token}"
	tests := []struct {
		name        string
		server      string
		user        string
		context     string
		insecure    bool
		ok          bool
		unreachable bool
		retryable   bool
	}{
		{name: "ready", server: serve(status(http.StatusOK)), ok: true},
		{name: "connection refused", server: refused, unreachable: true, retryable: true},
		{name: "timeout", server: serve(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })), unreachable: true, retryable: true},
		{name: "unknown host", server: "http://tack-test.invalid", unreachable: true, retryable: true},
		{name: "missing context", server: refused, context: "site-x"},
		{name: "401", server: serve(status(http.StatusUnauthorized))},
		{name: "403", server: serve(status(http.StatusForbidden))},
		{name: "500 not ready", server: serve(status(http.StatusInternalServerError)), retryable: true},
		{name: "503", server: serve(status(http.StatusServiceUnavailable)), retryable: true},
		{name: "429", server: serve(status(http.StatusTooManyRequests)), retryable: true},
		{name: "certificate from an unknown authority", server: tlsServer.URL},
		// client-go runs a credential plugin over TLS only.
		{name: "credential plugin fails", server: tlsServer.URL, insecure: true,
			user: `{exec: {apiVersion: client.authentication.k8s.io/v1, command: /nonexistent/tack-credential-plugin, interactiveMode: Never}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := tt.user
			if user == "" {
				user = token
			}
			ctxName := tt.context
			if ctxName == "" {
				ctxName = "site-a"
			}
			k := NewKubeconfig(kubeconfig(t, tt.server, user, tt.insecure), map[string]string{"a": ctxName})
			// Long enough for an answer on a busy host; the timeout case
			// waits for all of it.
			k.CheckTimeout = 2 * time.Second
			err := k.Reachable(context.Background(), "a")
			if tt.ok {
				if err != nil {
					t.Fatalf("err %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no error")
			}
			if got := IsUnreachable(err); got != tt.unreachable {
				t.Errorf("IsUnreachable = %v, want %v: %v", got, tt.unreachable, err)
			}
			if got := Retryable(err); got != tt.retryable {
				t.Errorf("Retryable = %v, want %v: %v", got, tt.retryable, err)
			}
		})
	}
}

// #35 row 32. Transient tells a ledger write to repeat: no answer, 5xx or
// 429. A refusal, a conflict or a certificate Tack does not trust is final.
func TestTransient(t *testing.T) {
	gr := schema.GroupResource{Group: "failover.tako.id", Resource: "sitefailovers"}
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"connection reset", &url.Error{Op: "Put", URL: "https://x", Err: syscall.ECONNRESET}, true},
		{"timeout", context.DeadlineExceeded, true},
		{"503", apierrors.NewServiceUnavailable("down"), true},
		{"500", apierrors.NewInternalError(errors.New("boom")), true},
		{"429", apierrors.NewTooManyRequests("slow down", 1), true},
		{"403", apierrors.NewForbidden(gr, "run-1", errors.New("no")), false},
		{"409", apierrors.NewConflict(gr, "run-1", errors.New("modified")), false},
		{"unknown authority", &url.Error{Op: "Put", URL: "https://x", Err: x509.UnknownAuthorityError{}}, false},
	} {
		if got := Transient(tt.err); got != tt.want {
			t.Errorf("%s: Transient %v, want %v", tt.name, got, tt.want)
		}
	}
}

// plugin writes a credential plugin that logs its start, with its pid, and
// its end to log, waits for wait, and then gives a token, or fails when
// fail is set.
func plugin(t *testing.T, log string, wait time.Duration, fail bool) string {
	t.Helper()
	end := `printf '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"t-%s"}}' "$1"`
	if fail {
		end = `echo "no login for $1" >&2; exit 1`
	}
	script := fmt.Sprintf("#!/bin/sh\necho \"start $(date +%%s%%N) $1 $$\" >>%q\nsleep %s\necho \"end $(date +%%s%%N) $1\" >>%q\n%s\n",
		log, strconv.FormatFloat(wait.Seconds(), 'f', 3, 64), log, end)
	path := filepath.Join(t.TempDir(), "plugin.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// sitesWithPlugin writes a kubeconfig whose contexts site-a and site-b
// point at server, each with the credential plugin of its site, and whose
// context site-c points at server with a token and no plugin.
func sitesWithPlugin(t *testing.T, server, plugin string) *Kubeconfig {
	t.Helper()
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: %q, insecure-skip-tls-verify: true}
users:
- name: a
  user: {exec: {apiVersion: client.authentication.k8s.io/v1, command: %q, args: [a], interactiveMode: Never}}
- name: b
  user: {exec: {apiVersion: client.authentication.k8s.io/v1, command: %q, args: [b], interactiveMode: Never}}
- name: c
  user: {token: t-c}
contexts:
- name: site-a
  context: {cluster: c, user: a}
- name: site-b
  context: {cluster: c, user: b}
- name: site-c
  context: {cluster: c, user: c}
`, server, plugin, plugin)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewKubeconfig(path, map[string]string{"a": "site-a", "b": "site-b", "c": "site-c"})
}

// pluginEvents waits until the plugin logged ends ends, and returns its
// events in order, each "start <site>" or "end <site>". A test that stops
// waiting for a plugin waits here for it to end: the plugin keeps the turn
// of the process until then.
func pluginEvents(t *testing.T, log string, ends int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(log)
		var events []string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if f := strings.Fields(l); len(f) >= 3 {
				events = append(events, f[0]+" "+f[2])
			}
		}
		n := 0
		for _, e := range events {
			if strings.HasPrefix(e, "end ") {
				n++
			}
		}
		if n >= ends {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("the plugin logged %d ends, want %d: %v", n, ends, events)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// #58 row 6, observed on kind: a credential plugin that waited for the
// operator ran inside the check's request, whose 5 s covered the wait, and
// the check counted as no answer: a live site could read as lost (#25).
// Its credentials are now obtained before the check, with no time limit
// ([RFC#0007] Requirement 12).
func TestCredentialsTakeNothingOfTheCheck(t *testing.T) {
	server := httptest.NewTLSServer(status(http.StatusOK))
	t.Cleanup(server.Close)
	log := filepath.Join(t.TempDir(), "plugin.log")
	k := sitesWithPlugin(t, server.URL, plugin(t, log, 1500*time.Millisecond, false))
	// The plugin takes longer than a check, and than any other request.
	k.CheckTimeout, k.requestTimeout = 500*time.Millisecond, time.Second
	if err := k.Reachable(context.Background(), "a"); err != nil {
		t.Fatalf("a site whose plugin takes longer than a check: %v (unreachable %v)", err, IsUnreachable(err))
	}
}

// Two credential plugins never run at once: one may prompt on the terminal
// while the other does, and their prompts mixed on kind (#58 row 6).
func TestCredentialPluginsRunOneAtATime(t *testing.T) {
	server := httptest.NewTLSServer(status(http.StatusOK))
	t.Cleanup(server.Close)
	log := filepath.Join(t.TempDir(), "plugin.log")
	k := sitesWithPlugin(t, server.URL, plugin(t, log, 300*time.Millisecond, false))
	var wg sync.WaitGroup
	for _, s := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := k.Reachable(context.Background(), s); err != nil {
				t.Errorf("site %s: %v", s, err)
			}
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(l)
		events = append(events, f[0]+" "+f[2])
	}
	// Each plugin ends before the other starts.
	if len(events) != 4 || strings.Fields(events[0])[0] != "start" || strings.Fields(events[1])[0] != "end" ||
		strings.Fields(events[0])[1] != strings.Fields(events[1])[1] {
		t.Errorf("the plugins overlapped: %v", events)
	}
}

// A site whose credentials cannot be obtained is not lost, and its plugin
// runs once in the process: every later check returns the same error.
func TestCredentialsThatFailAreKept(t *testing.T) {
	server := httptest.NewTLSServer(status(http.StatusOK))
	t.Cleanup(server.Close)
	log := filepath.Join(t.TempDir(), "plugin.log")
	k := sitesWithPlugin(t, server.URL, plugin(t, log, 0, true))
	for i := range 2 {
		err := k.Reachable(context.Background(), "a")
		if err == nil || IsUnreachable(err) || Retryable(err) ||
			!strings.Contains(err.Error(), `site "a": the credentials of context "site-a" could not be obtained, so the site is not counted as lost`) {
			t.Fatalf("check %d: %v, want a final error that is not no answer", i+1, err)
		}
	}
	if b, _ := os.ReadFile(log); strings.Count(string(b), "start") != 1 {
		t.Errorf("the plugin ran %d times, want 1", strings.Count(string(b), "start"))
	}
}

// Found in the coordinator's review of PR #68: client-go runs the
// credential plugin with no context, and Credentials waited for it
// whatever its ctx did: a ctx of 1 s returned after the plugin's 8 s, and
// a SIGTERM did not end tack. The end of ctx now ends the wait. The plugin
// runs on until it ends, and the interrupt is not kept as credentials that
// cannot be obtained ([RFC#0007] Requirement 12).
func TestCredentialsWaitEndsWithTheContext(t *testing.T) {
	server := httptest.NewTLSServer(status(http.StatusOK))
	t.Cleanup(server.Close)
	log := filepath.Join(t.TempDir(), "plugin.log")
	k := sitesWithPlugin(t, server.URL, plugin(t, log, time.Second, false))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := k.Credentials(ctx, "a")
	took := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || took > 700*time.Millisecond {
		t.Errorf("Credentials with a ctx that ended after 200 ms: %v after %s, want ctx's error at once", err, took.Round(time.Millisecond))
	}
	pluginEvents(t, log, 1)
	// The plugin ended after the interrupt, with credentials.
	if err := k.Credentials(context.Background(), "a"); err != nil {
		t.Errorf("Credentials after the interrupt: %v, want nil: an interrupt is not kept", err)
	}
	if err := k.Reachable(context.Background(), "a"); err != nil {
		t.Errorf("Reachable after the interrupt: %v", err)
	}
}

// Found in the coordinator's review of PR #68: a site waited for the turn
// behind another site's plugin whatever its ctx did, also a site with no
// plugin (7.7 s behind a plugin of 8 s). A site whose context has no
// credential plugin does not wait for the turn; a site with one waits,
// until its plugin may run or its ctx ends.
func TestOnlyPluginsWaitForTheTurn(t *testing.T) {
	server := httptest.NewTLSServer(status(http.StatusOK))
	t.Cleanup(server.Close)
	log := filepath.Join(t.TempDir(), "plugin.log")
	k := sitesWithPlugin(t, server.URL, plugin(t, log, time.Second, false))
	a := make(chan error, 1)
	go func() { a <- k.Credentials(context.Background(), "a") }()
	deadline := time.Now().Add(5 * time.Second)
	for b, _ := os.ReadFile(log); !strings.Contains(string(b), "start"); b, _ = os.ReadFile(log) {
		if time.Now().After(deadline) {
			t.Fatal("site a's plugin did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	start := time.Now()
	if err := k.Credentials(context.Background(), "c"); err != nil || time.Since(start) > 300*time.Millisecond {
		t.Errorf("site c, with no plugin, while a's plugin runs: %v after %s, want nil at once", err, time.Since(start).Round(time.Millisecond))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	if err := k.Credentials(ctx, "b"); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 700*time.Millisecond {
		t.Errorf("site b, with a plugin and a ctx of 200 ms, while a's plugin runs: %v after %s, want ctx's error at once", err, time.Since(start).Round(time.Millisecond))
	}
	if err := <-a; err != nil {
		t.Fatalf("site a: %v", err)
	}
	if events := pluginEvents(t, log, 1); len(events) != 2 {
		t.Errorf("plugins run while b waited for the turn: %v, want a's only", events)
	}
	if err := k.Credentials(context.Background(), "b"); err != nil {
		t.Errorf("site b once a's plugin ended: %v", err)
	}
	if err := k.Reachable(context.Background(), "c"); err != nil {
		t.Errorf("site c: %v", err)
	}
}

// A plugin whose caller stopped waiting keeps the turn until it ends:
// another site's plugin never starts while it runs, and may prompt on
// the same terminal.
func TestPluginKeepsTheTurnUntilItEnds(t *testing.T) {
	server := httptest.NewTLSServer(status(http.StatusOK))
	t.Cleanup(server.Close)
	log := filepath.Join(t.TempDir(), "plugin.log")
	k := sitesWithPlugin(t, server.URL, plugin(t, log, 600*time.Millisecond, false))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := k.Credentials(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("site a with a ctx of 100 ms: %v, want ctx's error", err)
	}
	if err := k.Credentials(context.Background(), "b"); err != nil {
		t.Fatalf("site b: %v", err)
	}
	if events := pluginEvents(t, log, 2); strings.Join(events, ", ") != "start a, end a, start b, end b" {
		t.Errorf("the plugins ran %v, want a's, then b's", events)
	}
}
