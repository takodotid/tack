// Package kube builds one Kubernetes client per site, each from an
// explicit kubeconfig context.
package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
)

// FieldManager is the field manager of every write by Tack: to live
// objects, to Flux's objects, to the ledger and its Events.
const FieldManager = "tack"

// RequestTimeout bounds every request to a site, so a lost site fails fast.
const RequestTimeout = 15 * time.Second

// CheckTimeout bounds one check of a site's API.
const CheckTimeout = 5 * time.Second

// Scheme knows the core types and the ledger.
var Scheme = NewScheme()

// NewScheme returns a scheme that knows the core types and the ledger. A
// fake client of controller-runtime adds the list kinds of unstructured
// objects to its scheme as it meets them, under a lock of its own: each
// fake needs a scheme of its own.
func NewScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := failoverv1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// Clients gives the client of each site.
type Clients interface {
	// Client returns the site's client.
	Client(site string) (client.Client, error)
	// Reachable returns nil when the site's API server answers that it is
	// ready. An error that IsUnreachable accepts means the site gave no
	// answer; any other error means it answered, or could not be checked.
	Reachable(ctx context.Context, site string) error
}

// Credentialed is a Clients that obtains a site's credentials apart from
// its checks ([RFC#0007] Requirement 12).
type Credentialed interface {
	// Credentials obtains the credentials of the site's kube context, with
	// no time limit, sending nothing to the site. An error means the site
	// could not be checked, never that it gave no answer.
	Credentials(ctx context.Context, site string) error
}

// UnreachableError is a check of a site's API that got no answer: no
// connection, a reset, a DNS failure, or a timeout. Only such a check
// counts toward a lost site ([RFC#0007] Requirement 5).
type UnreachableError struct{ Err error }

func (e *UnreachableError) Error() string { return e.Err.Error() }
func (e *UnreachableError) Unwrap() error { return e.Err }

// IsUnreachable reports whether err is a check of a site's API that got no
// answer. A context missing from the kubeconfig, a credential the site
// refuses, a certificate Tack does not trust, or any HTTP answer is not.
func IsUnreachable(err error) bool {
	var u *UnreachableError
	return errors.As(err, &u)
}

// Retryable reports whether a failed check is worth repeating: it got no
// answer, or the API server answered that it is not ready yet (5xx, 429).
// A 5xx or 429 still never counts as no answer.
func Retryable(err error) bool {
	if IsUnreachable(err) {
		return true
	}
	var st apierrors.APIStatus
	if errors.As(err, &st) {
		code := st.Status().Code
		return code >= 500 || code == 429
	}
	return false
}

// Transient reports whether a failed request is worth repeating: it got
// no answer, or the API server answered that it cannot serve it now (5xx,
// 429).
func Transient(err error) bool {
	if noAnswer(err) {
		return true
	}
	var st apierrors.APIStatus
	if errors.As(err, &st) {
		code := st.Status().Code
		return code >= 500 || code == 429
	}
	return false
}

// NoAnswer reports whether a failed request got no answer from the server,
// as a check that IsUnreachable accepts.
func NoAnswer(err error) bool { return noAnswer(err) }

// noAnswer reports whether a failed request got no answer from the server.
// An unknown error counts as an answer: only a known transport failure may
// make a site count as lost.
func noAnswer(err error) bool {
	var (
		st      apierrors.APIStatus
		verify  *tls.CertificateVerificationError
		unknown x509.UnknownAuthorityError
		host    x509.HostnameError
		invalid x509.CertificateInvalidError
		record  tls.RecordHeaderError
	)
	switch {
	case errors.As(err, &st):
		// The server answered with an HTTP status.
		return false
	case errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &host),
		errors.As(err, &invalid), errors.As(err, &record):
		// The server answered with a certificate Tack does not trust, or
		// not with TLS: the kubeconfig does not match the server.
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) {
		// "remote error" is a TLS alert the server sent, such as a refused
		// client certificate.
		return op.Op != "remote error"
	}
	var dns *net.DNSError
	var ne net.Error
	switch {
	case errors.As(err, &dns):
		return true
	case errors.As(err, &ne) && ne.Timeout():
		return true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return true
	}
	return false
}

// Kubeconfig builds clients from a kubeconfig file.
type Kubeconfig struct {
	// Path is the kubeconfig file. Empty uses the default loading rules.
	Path string
	// Contexts maps each site to its context.
	Contexts map[string]string

	// CheckTimeout bounds one check of a site's API. Zero uses CheckTimeout.
	CheckTimeout time.Duration
	// requestTimeout bounds every other request. Zero uses RequestTimeout.
	requestTimeout time.Duration

	mu      sync.Mutex
	clients map[string]client.Client
	configs map[string]*rest.Config
	// probes are the clients of Credentials, by site: nil for a context
	// without a credential plugin.
	probes map[string]rest.Interface
	// denied holds, by site, the credentials that could not be obtained:
	// every later check of the site returns it.
	denied map[string]error
}

// pluginTurn runs one credential plugin at a time in the process: a
// plugin may prompt on the terminal ([RFC#0007] Requirement 12). A probe
// takes it before its plugin starts and gives it back once its plugin
// ended, also when its caller stopped waiting.
var pluginTurn = make(chan struct{}, 1)

// localAnswer answers every request itself. The probe of Credentials goes
// through the credential plugin's wrapper of client-go, which runs the
// plugin, and ends here, before the network.
type localAnswer struct{}

func (localAnswer) RoundTrip(req *http.Request) (*http.Response, error) {
	// An interrupt during the plugin is not credentials obtained.
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader("ok")), Request: req,
	}, nil
}

// Credentials implements Credentialed. It runs the context's credential
// plugin, when it has one and client-go holds no valid credentials from
// it, through a request that never leaves the process. client-go keeps one
// plugin per configuration, so the site's other clients use what it got.
// A context without a plugin has nothing to obtain. Credentials run one at
// a time in the process. The end of ctx ends the wait, for the turn and
// for the plugin: client-go runs the plugin with no context, so the plugin
// itself runs on until it ends, and keeps the turn until then. Credentials
// that cannot be obtained are kept: every later check of the site returns
// that error. An interrupt is not kept.
func (k *Kubeconfig) Credentials(ctx context.Context, site string) error {
	k.mu.Lock()
	if err := k.denied[site]; err != nil {
		k.mu.Unlock()
		return err
	}
	probe, err := k.probe(site)
	k.mu.Unlock()
	if err != nil {
		return fmt.Errorf("%w; a site that cannot be checked is not counted as lost", err)
	}
	if probe == nil {
		return nil
	}
	select {
	case pluginTurn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		defer func() { <-pluginTurn }()
		done <- probe.Get().AbsPath("/readyz").Do(ctx).Error()
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
	}
	// An interrupt is not credentials that cannot be obtained: it is not
	// kept.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	err = fmt.Errorf("site %q: the credentials of context %q could not be obtained, so the site is not counted as lost: %w", site, k.Contexts[site], err)
	k.mu.Lock()
	if k.denied == nil {
		k.denied = map[string]error{}
	}
	k.denied[site] = err
	k.mu.Unlock()
	return err
}

// probe returns the client of Credentials for site: its context's
// configuration, with no time limit, whose requests end at localAnswer.
// It returns nil for a context without a credential plugin, exec or
// auth-provider. k.mu must be held.
func (k *Kubeconfig) probe(site string) (rest.Interface, error) {
	if p, ok := k.probes[site]; ok {
		return p, nil
	}
	cfg, err := k.restConfig(site)
	if err != nil {
		return nil, err
	}
	var p rest.Interface
	if cfg.ExecProvider != nil || cfg.AuthProvider != nil {
		cfg = rest.CopyConfig(cfg)
		cfg.Timeout = 0
		cfg.WrapTransport = func(http.RoundTripper) http.RoundTripper { return localAnswer{} }
		p, err = rest.UnversionedRESTClientFor(setDefaults(cfg))
		if err != nil {
			return nil, fmt.Errorf("site %q: %w", site, err)
		}
	}
	if k.probes == nil {
		k.probes = map[string]rest.Interface{}
	}
	k.probes[site] = p
	return p, nil
}

// NewKubeconfig returns clients for the sites' contexts.
func NewKubeconfig(path string, contexts map[string]string) *Kubeconfig {
	return &Kubeconfig{Path: path, Contexts: contexts}
}

func (k *Kubeconfig) restConfig(site string) (*rest.Config, error) {
	if c, ok := k.configs[site]; ok {
		return c, nil
	}
	name, ok := k.Contexts[site]
	if !ok || name == "" {
		return nil, fmt.Errorf("site %q has no kube context", site)
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if k.Path != "" {
		rules.ExplicitPath = k.Path
	}
	raw, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("site %q: load kubeconfig: %w", site, err)
	}
	if _, ok := raw.Contexts[name]; !ok {
		return nil, fmt.Errorf("site %q: context %q is not in the kubeconfig", site, name)
	}
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, name, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("site %q: context %q: %w", site, name, err)
	}
	cfg.Timeout = RequestTimeout
	if k.requestTimeout > 0 {
		cfg.Timeout = k.requestTimeout
	}
	cfg.QPS, cfg.Burst = 20, 40
	if k.configs == nil {
		k.configs = map[string]*rest.Config{}
	}
	k.configs[site] = cfg
	return cfg, nil
}

// Client implements Clients.
func (k *Kubeconfig) Client(site string) (client.Client, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if c, ok := k.clients[site]; ok {
		return c, nil
	}
	cfg, err := k.restConfig(site)
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: Scheme})
	if err != nil {
		return nil, fmt.Errorf("site %q: %w", site, err)
	}
	if k.clients == nil {
		k.clients = map[string]client.Client{}
	}
	k.clients[site] = c
	return c, nil
}

// Reachable implements Clients. It obtains the site's credentials first,
// with no time limit, so a credential plugin that waits, such as for the
// operator, takes nothing of the check's time. Only a check that gets no
// answer returns an UnreachableError.
func (k *Kubeconfig) Reachable(ctx context.Context, site string) error {
	if err := k.Credentials(ctx, site); err != nil {
		return err
	}
	k.mu.Lock()
	cfg, err := k.restConfig(site)
	k.mu.Unlock()
	if err != nil {
		return fmt.Errorf("%w; a site that cannot be checked is not counted as lost", err)
	}
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = CheckTimeout
	if k.CheckTimeout > 0 {
		cfg.Timeout = k.CheckTimeout
	}
	rc, err := rest.UnversionedRESTClientFor(setDefaults(cfg))
	if err != nil {
		return fmt.Errorf("site %q: %w; a site that cannot be checked is not counted as lost", site, err)
	}
	err = rc.Get().AbsPath("/readyz").Do(ctx).Error()
	switch {
	case err == nil:
		return nil
	case noAnswer(err):
		return fmt.Errorf("site %q is unreachable: %w", site, &UnreachableError{Err: err})
	}
	return fmt.Errorf("site %q answered its check with an error, so it is not counted as lost: %w", site, err)
}

// Proxier is a Clients that can read one HTTP path of a pod through the
// site's API server ([RFC#0001] Requirement 2). It sends a GET only.
type Proxier interface {
	// ProxyGet returns the body of a GET of path on the pod's port, through
	// the API server's pods/proxy subresource, with scheme http or https.
	ProxyGet(ctx context.Context, site, namespace, pod, scheme string, port int, path string) ([]byte, error)
}

// ProxyGet implements Proxier. Only the cnpg driver calls it, for the
// instance manager's status and a replica's metrics ([RFC#0004]
// Requirement 13).
func (k *Kubeconfig) ProxyGet(ctx context.Context, site, namespace, pod, scheme string, port int, path string) ([]byte, error) {
	k.mu.Lock()
	cfg, err := k.restConfig(site)
	k.mu.Unlock()
	if err != nil {
		return nil, err
	}
	rc, err := rest.UnversionedRESTClientFor(setDefaults(rest.CopyConfig(cfg)))
	if err != nil {
		return nil, fmt.Errorf("site %q: %w", site, err)
	}
	target := fmt.Sprintf("%s:%s:%d", scheme, pod, port)
	return rc.Get().AbsPath("/api/v1/namespaces", namespace, "pods", target, "proxy", strings.TrimPrefix(path, "/")).DoRaw(ctx)
}

// Server implements the optional Server of Clients: the API server URL of
// the site's context.
func (k *Kubeconfig) Server(site string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	cfg, err := k.restConfig(site)
	if err != nil {
		return "", err
	}
	return cfg.Host, nil
}

// Fingerprint returns the UID of the cluster's kube-system namespace. It
// tells one cluster from another whatever the kubeconfig calls them.
func Fingerprint(ctx context.Context, c client.Client) (string, error) {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: metav1.NamespaceSystem}, ns); err != nil {
		return "", fmt.Errorf("read namespace %s, which identifies the site's cluster: %w", metav1.NamespaceSystem, err)
	}
	if ns.UID == "" {
		return "", fmt.Errorf("namespace %s has no UID", metav1.NamespaceSystem)
	}
	return string(ns.UID), nil
}

func setDefaults(cfg *rest.Config) *rest.Config {
	cfg.NegotiatedSerializer = clientgoscheme.Codecs.WithoutConversion()
	return cfg
}

// Contexts maps each site of a workload to its kube context.
func Contexts(s *failoverv1.WorkloadSpec) map[string]string {
	m := map[string]string{}
	for _, site := range s.Sites {
		m[site.Name] = site.KubeContext
	}
	return m
}
