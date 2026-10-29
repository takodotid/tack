// Package testenv holds fakes of the sites, Flux and Git for unit tests.
package testenv

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/stop"
)

// GVKs the fakes know besides the core types.
var GVKs = []schema.GroupVersionKind{
	{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"},
	{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Pooler"},
	{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"},
	{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"},
	{Group: "strait.tako.id", Version: "v1alpha1", Kind: "SiteService"},
	{Group: "apps", Version: "v1", Kind: "Deployment"},
	{Group: "failover.tako.id", Version: "v1", Kind: "SiteFailover"},
}

// Clients is a fake kube.Clients with one fake client per site.
type Clients struct {
	mu    sync.Mutex
	Sites map[string]client.Client
	Down  map[string]bool
	// Writes counts the writes made through Client.
	Writes int
}

// NewClients returns fake clients for the sites, seeded with objects.
// Writes through Client are counted in Writes.
func NewClients(objects map[string][]client.Object) *Clients {
	c := &Clients{Sites: map[string]client.Client{}, Down: map[string]bool{}}
	for site, objs := range objects {
		c.Sites[site] = NewClient(objs...)
	}
	return c
}

func (c *Clients) counted(cl client.Client) client.Client {
	count := func() { c.mu.Lock(); c.Writes++; c.mu.Unlock() }
	return interceptor.NewClient(cl.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			count()
			return cl.Create(ctx, o, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			count()
			return cl.Update(ctx, o, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption) error {
			count()
			return cl.Patch(ctx, o, p, opts...)
		},
		Delete: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			count()
			return cl.Delete(ctx, o, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
			count()
			return cl.SubResource(sub).Update(ctx, o, opts...)
		},
	})
}

// NewClient returns a fake client that knows the GVKs.
func NewClient(objs ...client.Object) client.Client {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range GVKs {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return fake.NewClientBuilder().
		WithScheme(kube.Scheme).
		WithRESTMapper(mapper).
		WithObjects(objs...).
		WithStatusSubresource(&failoverv1.SiteFailover{}).
		Build()
}

// SetDown marks a site unreachable or back.
func (c *Clients) SetDown(site string, down bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Down[site] = down
}

// Client implements kube.Clients.
func (c *Clients) Client(site string) (client.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Down[site] {
		return nil, fmt.Errorf("site %s is unreachable", site)
	}
	cl, ok := c.Sites[site]
	if !ok {
		return nil, fmt.Errorf("site %q has no fake client", site)
	}
	return c.counted(cl), nil
}

// Reachable implements kube.Clients.
func (c *Clients) Reachable(_ context.Context, site string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Down[site] {
		return fmt.Errorf("site %s is unreachable: connection refused", site)
	}
	return nil
}

// Object builds an unstructured object.
func Object(apiVersion, kind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	for k, v := range fields {
		u.Object[k] = v
	}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

// Flux is a fake engine.Flux. Every object is applied by Kustomization
// flux-system/<site>.
type Flux struct {
	mu       sync.Mutex
	Revision string
	Held     map[string]string
	Calls    []string
	// Fail makes a method return an error, by method name.
	Fail map[string]error
}

// NewFlux returns a fake Flux at a revision.
func NewFlux(rev string) *Flux {
	return &Flux{Revision: rev, Held: map[string]string{}, Fail: map[string]error{}}
}

func (f *Flux) call(name string, a ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, name+" "+fmt.Sprint(a...))
	return f.Fail[name]
}

func key(site string, o failoverv1.ObjectRef) string { return site + "/" + o.Kind + "/" + o.Name }

// Owner implements engine.Flux.
func (f *Flux) Owner(_ context.Context, site string, _ failoverv1.ObjectRef) (engine.Owner, error) {
	return engine.Owner{Kustomization: &failoverv1.ObjectKey{Namespace: "flux-system", Name: site}}, nil
}

// Hold implements engine.Flux.
func (f *Flux) Hold(_ context.Context, site string, obj failoverv1.ObjectRef, run, takeOver string) (failoverv1.ObjectKey, error) {
	if err := f.call("Hold", site, " ", obj.Name); err != nil {
		return failoverv1.ObjectKey{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.Held[key(site, obj)]; ok && h != run && h != takeOver {
		return failoverv1.ObjectKey{}, stop.Ask(failoverv1.ReasonHeldByAnotherRun, "held by %s", h)
	}
	f.Held[key(site, obj)] = run
	return failoverv1.ObjectKey{Namespace: "flux-system", Name: site}, nil
}

// Release implements engine.Flux.
func (f *Flux) Release(_ context.Context, site string, obj failoverv1.ObjectRef, run string) (int64, error) {
	if err := f.call("Release", site, " ", obj.Name); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Held[key(site, obj)] == run {
		delete(f.Held, key(site, obj))
	}
	return 1, nil
}

// Reconcile implements engine.Flux.
func (f *Flux) Reconcile(_ context.Context, site string, _ []failoverv1.ObjectKey, revision string, _ time.Duration) (string, []string, error) {
	if err := f.call("Reconcile", site, " ", revision); err != nil {
		return "", nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if revision != "" {
		return "main@sha1:" + revision, nil, nil
	}
	return f.Revision, nil, nil
}

// VerifyGit implements engine.Flux.
func (f *Flux) VerifyGit(_ context.Context, site string, held []failoverv1.HeldObject, _ []failoverv1.ObjectField, _ string, _ bool) (string, error) {
	return "Git equals live", f.call("VerifyGit", site)
}

// VerifyNoop implements engine.Flux.
func (f *Flux) VerifyNoop(_ context.Context, site string, _ []failoverv1.HeldObject, _ []failoverv1.ObjectField, _ bool) (string, error) {
	return "nothing changed", f.call("VerifyNoop", site)
}

// Git is a fake engine.Git. It maps each field to clusters/<site>/<kind>.yaml.
type Git struct {
	mu          sync.Mutex
	Unreachable bool
	Head        string
	Commits     []engine.CommitRequest
}

// Resolve implements engine.Git.
func (g *Git) Resolve(_ context.Context, _ *failoverv1.WorkloadSpec, changes []driver.FieldChange) ([]failoverv1.GitChange, error) {
	var out []failoverv1.GitChange
	for _, c := range changes {
		gc := failoverv1.GitChange{Site: c.Site, Object: c.Object, Field: c.Field, Value: c.Value}
		if !g.Unreachable {
			gc.File = fmt.Sprintf("clusters/%s/%s.yaml", c.Site, lower(c.Object.Kind))
			gc.Path = c.Field
		}
		out = append(out, gc)
	}
	if g.Unreachable {
		return out, fmt.Errorf("git: remote unreachable")
	}
	return out, nil
}

// Commit implements engine.Git.
func (g *Git) Commit(_ context.Context, _ *failoverv1.WorkloadSpec, req engine.CommitRequest) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Unreachable {
		return "", stop.Wait(failoverv1.ReasonGitUnreachable, "remote unreachable")
	}
	if g.Head != "" && g.Head != req.Base {
		return "", stop.Ask(failoverv1.ReasonGitChanged, "branch moved from %s to %s", req.Base, g.Head)
	}
	g.Commits = append(g.Commits, req)
	g.Head = fmt.Sprintf("c%039d", len(g.Commits))
	return g.Head, nil
}

// Checkout implements engine.Git.
func (g *Git) Checkout(context.Context, *failoverv1.WorkloadSpec, string) (string, error) {
	return "/nonexistent", nil
}

// Close implements engine.Git.
func (g *Git) Close() error { return nil }

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
