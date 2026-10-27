// Package kube builds one Kubernetes client per site, each from an
// explicit kubeconfig context.
package kube

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/api/v1alpha1"
)

// RequestTimeout bounds every request to a site, so a lost site fails fast.
const RequestTimeout = 15 * time.Second

// Scheme knows the core types and the ledger.
var Scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}()

// Clients gives the client of each site.
type Clients interface {
	// Client returns the site's client.
	Client(site string) (client.Client, error)
	// Reachable returns nil when the site's API server answers.
	Reachable(ctx context.Context, site string) error
}

// Kubeconfig builds clients from a kubeconfig file.
type Kubeconfig struct {
	// Path is the kubeconfig file. Empty uses the default loading rules.
	Path string
	// Contexts maps each site to its context.
	Contexts map[string]string

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

// Reachable implements Clients.
func (k *Kubeconfig) Reachable(ctx context.Context, site string) error {
	k.mu.Lock()
	cfg, err := k.restConfig(site)
	k.mu.Unlock()
	if err != nil {
		return err
	}
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = 5 * time.Second
	rc, err := rest.UnversionedRESTClientFor(setDefaults(cfg))
	if err != nil {
		return err
	}
	if err := rc.Get().AbsPath("/readyz").Do(ctx).Error(); err != nil {
		return fmt.Errorf("site %q is unreachable: %w", site, err)
	}
	return nil
}

func setDefaults(cfg *rest.Config) *rest.Config {
	cfg.NegotiatedSerializer = clientgoscheme.Codecs.WithoutConversion()
	return cfg
}

// Contexts maps each site of a workload to its kube context.
func Contexts(s *v1alpha1.WorkloadSpec) map[string]string {
	m := map[string]string{}
	for _, site := range s.Sites {
		m[site.Name] = site.KubeContext
	}
	return m
}
