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

// RequestTimeout bounds every request to a site, so a lost site fails fast.
const RequestTimeout = 15 * time.Second

// CheckTimeout bounds one check of a site's API.
const CheckTimeout = 5 * time.Second

// Scheme knows the core types and the ledger.
var Scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := failoverv1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}()

// Clients gives the client of each site.
type Clients interface {
	// Client returns the site's client.
	Client(site string) (client.Client, error)
	// Reachable returns nil when the site's API server answers that it is
	// ready. An error that IsUnreachable accepts means the site gave no
	// answer; any other error means it answered, or could not be checked.
	Reachable(ctx context.Context, site string) error
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

	mu      sync.Mutex
	clients map[string]client.Client
	configs map[string]*rest.Config
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

// Reachable implements Clients. Only a check that gets no answer returns
// an UnreachableError.
func (k *Kubeconfig) Reachable(ctx context.Context, site string) error {
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
