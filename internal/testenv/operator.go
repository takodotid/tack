package testenv

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Operator acts like CloudNativePG on the fake clients, for the Clusters
// pg-<site> and Poolers pg-<site>-pooler-rw in namespace db.
type Operator struct {
	Clients *Clients
	Sites   []string
	tokens  int
}

// Run reconciles until ctx ends.
func (o *Operator) Run(ctx context.Context) {
	for {
		for _, s := range o.Sites {
			o.reconcile(ctx, s)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (o *Operator) reconcile(ctx context.Context, site string) {
	if o.Clients.Reachable(ctx, site) != nil {
		return
	}
	c, err := o.Clients.Client(site)
	if err != nil {
		return
	}
	u := Object("postgresql.cnpg.io/v1", "Cluster", "db", "pg-"+site, nil)
	if c.Get(ctx, client.ObjectKeyFromObject(u), u) != nil {
		return
	}
	self := "pg-" + site
	primary, _, _ := unstructured.NestedString(u.Object, "spec", "replica", "primary")
	token, _, _ := unstructured.NestedString(u.Object, "spec", "replica", "promotionToken")
	last, _, _ := unstructured.NestedString(u.Object, "status", "lastPromotionToken")
	demotion, _, _ := unstructured.NestedString(u.Object, "status", "demotionToken")
	isPrimary, _, _ := unstructured.NestedBool(u.Object, "status", "instancesReportedState", self+"-1", "isPrimary")
	before := u.DeepCopy()
	switch {
	case primary != self && isPrimary:
		o.tokens++
		set(u, fmt.Sprintf("token-%s-%d", site, o.tokens), "status", "demotionToken")
		set(u, false, "status", "instancesReportedState", self+"-1", "isPrimary")
	case primary == self && !isPrimary && (token == "" || token != last):
		if token != "" {
			set(u, token, "status", "lastPromotionToken")
		}
		set(u, true, "status", "instancesReportedState", self+"-1", "isPrimary")
		set(u, "", "status", "demotionToken")
	case primary == self && token == "" && last != "":
		set(u, "", "status", "lastPromotionToken")
	case primary == self && demotion != "":
		set(u, "", "status", "demotionToken")
	}
	if fmt.Sprint(before.Object) != fmt.Sprint(u.Object) {
		_ = c.Update(ctx, u)
	}

	p := Object("postgresql.cnpg.io/v1", "Pooler", "db", "pg-"+site+"-pooler-rw", nil)
	if c.Get(ctx, client.ObjectKeyFromObject(p), p) != nil {
		return
	}
	n, _, _ := unstructured.NestedInt64(p.Object, "spec", "instances")
	d := Object("apps/v1", "Deployment", "db", "pg-"+site+"-pooler-rw", nil)
	err = c.Get(ctx, client.ObjectKeyFromObject(d), d)
	if err != nil {
		d = Object("apps/v1", "Deployment", "db", "pg-"+site+"-pooler-rw", map[string]any{"spec": map[string]any{}})
		_ = c.Create(ctx, d)
	}
	status := map[string]any{}
	if n > 0 {
		status = map[string]any{"replicas": n, "readyReplicas": n}
	}
	if fmt.Sprint(d.Object["status"]) != fmt.Sprint(status) {
		d.Object["status"] = status
		_ = c.Status().Update(ctx, d)
	}
}

func set(u *unstructured.Unstructured, v any, path ...string) {
	if v == "" {
		unstructured.RemoveNestedField(u.Object, path...)
		return
	}
	_ = unstructured.SetNestedField(u.Object, v, path...)
}
