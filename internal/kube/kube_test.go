package kube

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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
