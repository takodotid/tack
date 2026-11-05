// Package testenv holds fakes of the sites, Flux and Git for unit tests.
package testenv

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
	{Group: "strait.tako.id", Version: "v1", Kind: "SiteService"},
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
	// Checks counts the calls to Reachable, by site.
	Checks map[string]int
	// answers holds, by site, the results of the next checks: false fails.
	answers map[string][]bool
	// errs holds, by site, an error every check returns, such as a refused
	// credential.
	errs map[string]error
}

// NewClients returns fake clients for the sites, seeded with objects. Each
// site's cluster has a kube-system namespace with UID uid-<site>, unless
// objects hold one. Writes through Client are counted in Writes.
func NewClients(objects map[string][]client.Object) *Clients {
	c := &Clients{Sites: map[string]client.Client{}, Down: map[string]bool{}, Checks: map[string]int{}, answers: map[string][]bool{}, errs: map[string]error{}}
	for site, objs := range objects {
		has := false
		for _, o := range objs {
			has = has || o.GetName() == metav1.NamespaceSystem && o.GetNamespace() == "" && o.GetObjectKind().GroupVersionKind().Kind == "Namespace"
		}
		if !has {
			objs = append(objs, KubeSystem("uid-"+site))
		}
		c.Sites[site] = NewClient(objs...)
	}
	return c
}

// KubeSystem returns a kube-system namespace with uid, which fingerprints
// a cluster.
func KubeSystem(uid string) *corev1.Namespace {
	ns := &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}}
	ns.Name, ns.UID = metav1.NamespaceSystem, types.UID(uid)
	return ns
}

// Server implements the optional Server of kube.Clients.
func (c *Clients) Server(site string) (string, error) {
	return "https://" + site + ".example.invalid:6443", nil
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
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Namespace"), meta.RESTScopeRoot)
	return fake.NewClientBuilder().
		WithScheme(kube.Scheme).
		WithRESTMapper(mapper).
		WithObjects(objs...).
		WithStatusSubresource(&failoverv1.SiteFailover{}).
		Build()
}

// SetSite makes a site's client c, such as one for another cluster.
func (c *Clients) SetSite(site string, cl client.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Sites[site] = cl
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

// IsDown tells whether a site is lost. Unlike Reachable, it is neither
// counted nor failed by a blip.
func (c *Clients) IsDown(site string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Down[site]
}

// Blip makes the next n checks of a site fail, as a short network cut
// between the operator's host and the site does.
func (c *Clients) Blip(site string, n int) {
	c.Flap(site, make([]bool, n)...)
}

// Flap sets the results of the next checks of a site: false fails.
func (c *Clients) Flap(site string, answers ...bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answers[site] = answers
}

// Fail makes every check of a site return err, as a site whose API
// refuses the operator's credential answers. Nil clears it.
func (c *Clients) Fail(site string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs[site] = err
}

// Reachable implements kube.Clients. A site that is down or blips gives no
// answer: its error is a kube.UnreachableError.
func (c *Clients) Reachable(_ context.Context, site string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Checks[site]++
	if err := c.errs[site]; err != nil {
		return err
	}
	if c.Down[site] {
		return fmt.Errorf("site %s is unreachable: %w", site, &kube.UnreachableError{Err: errors.New("connection refused")})
	}
	if a := c.answers[site]; len(a) > 0 {
		c.answers[site] = a[1:]
		if !a[0] {
			return fmt.Errorf("site %s is unreachable: %w", site, &kube.UnreachableError{Err: errors.New("context deadline exceeded")})
		}
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
	// GitDown makes a Git fetch hang for half the timeout, as a
	// GitRepository does when it cannot reach Git.
	GitDown bool
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

// HeldBy implements engine.Flux.
func (f *Flux) HeldBy(_ context.Context, site string, obj failoverv1.ObjectRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Held[key(site, obj)], nil
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

// Reconcile implements engine.Flux. Calls records "fetch" or "no-fetch"
// after the site.
func (f *Flux) Reconcile(ctx context.Context, site string, _ []failoverv1.ObjectKey, revision string, fetchGit bool, timeout time.Duration) (string, []string, error) {
	fetch := "no-fetch"
	if fetchGit {
		fetch = "fetch"
	}
	if err := f.call("Reconcile", site, " ", fetch, " ", revision); err != nil {
		return "", nil, err
	}
	var notes []string
	f.mu.Lock()
	down := f.GitDown
	f.mu.Unlock()
	if fetchGit && down {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(timeout / 2):
		}
		notes = append(notes, "GitRepository did not handle the reconcile request in "+(timeout/2).String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if revision != "" {
		return "main@sha1:" + revision, notes, nil
	}
	return f.Revision, notes, nil
}

// VerifyGit implements engine.Flux.
func (f *Flux) VerifyGit(_ context.Context, site string, held []failoverv1.HeldObject, _ []failoverv1.ObjectField, _ string, _ bool) (string, error) {
	return "Git equals live", f.call("VerifyGit", site)
}

// VerifyNoop implements engine.Flux.
func (f *Flux) VerifyNoop(_ context.Context, site string, _ []failoverv1.HeldObject, _ []failoverv1.ObjectField, _ bool, _ time.Duration) (string, error) {
	return "nothing changed", f.call("VerifyNoop", site)
}

// Git is a fake engine.Git. It maps each field to clusters/<site>/<kind>.yaml.
// The branch's history is the commits Commit made, in order; a Head set by
// a test on top of them is a commit pushed after them.
type Git struct {
	mu          sync.Mutex
	Unreachable bool
	Head        string
	Commits     []engine.CommitRequest
	// Checkouts are the revisions Checkout was asked for.
	Checkouts []string
	made      []string
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
	g.made = append(g.made, g.Head)
	return g.Head, nil
}

// Checkout implements engine.Git.
func (g *Git) Checkout(_ context.Context, _ *failoverv1.WorkloadSpec, rev string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Checkouts = append(g.Checkouts, rev)
	return "/nonexistent", nil
}

// BranchHead implements engine.Git.
func (g *Git) BranchHead(context.Context, *failoverv1.WorkloadSpec) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Unreachable {
		return "", stop.Wait(failoverv1.ReasonGitUnreachable, "remote unreachable")
	}
	return g.Head, nil
}

// Contains implements engine.Git. Head, when Commit did not make it, comes
// after every commit Commit made.
func (g *Git) Contains(_ context.Context, _ *failoverv1.WorkloadSpec, commit, rev string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if commit == rev {
		return true, nil
	}
	order := g.made
	if g.Head != "" && !slices.Contains(order, g.Head) {
		order = append(slices.Clone(order), g.Head)
	}
	i, j := slices.Index(order, commit), slices.Index(order, rev)
	return i >= 0 && j >= 0 && i <= j, nil
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
