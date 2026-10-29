// Package ledger stores SiteFailover objects in one site's cluster.
package ledger

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
)

// Store keeps the SiteFailover objects of one namespace at one site.
type Store struct {
	Client    client.Client
	Namespace string
	// Selector limits List, such as to one workload.
	Selector map[string]string
}

// Create implements engine.Ledger. It fails when the name is taken.
func (s *Store) Create(ctx context.Context, sf *failoverv1.SiteFailover) error {
	sf.Namespace = s.Namespace
	return served(s.Client.Create(ctx, sf))
}

// Get implements engine.Ledger.
func (s *Store) Get(ctx context.Context, name string) (*failoverv1.SiteFailover, error) {
	sf := &failoverv1.SiteFailover{}
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, sf); err != nil {
		return nil, served(err)
	}
	return sf, nil
}

// UpdateStatus implements engine.Ledger. The object's resourceVersion makes
// the write fail when another writer changed it since it was read.
func (s *Store) UpdateStatus(ctx context.Context, sf *failoverv1.SiteFailover) error {
	if sf.ResourceVersion == "" {
		return fmt.Errorf("ledger %s has no resourceVersion", sf.Name)
	}
	return served(s.Client.Status().Update(ctx, sf))
}

// List implements engine.Ledger, newest first.
func (s *Store) List(ctx context.Context) ([]failoverv1.SiteFailover, error) {
	var l failoverv1.SiteFailoverList
	opts := []client.ListOption{client.InNamespace(s.Namespace)}
	if len(s.Selector) > 0 {
		opts = append(opts, client.MatchingLabels(s.Selector))
	}
	if err := s.Client.List(ctx, &l, opts...); err != nil {
		return nil, served(err)
	}
	sort.Slice(l.Items, func(i, j int) bool {
		return l.Items[j].CreationTimestamp.Before(&l.Items[i].CreationTimestamp)
	})
	return l.Items, nil
}

// served explains an error from a cluster whose CRD does not serve this
// version, such as one still on the CRD of v0.1.x.
func served(err error) error {
	if meta.IsNoMatchError(err) {
		return fmt.Errorf("the cluster does not serve SiteFailover %s: install or upgrade Tack's CRD there: %w", failoverv1.GroupVersion, err)
	}
	return err
}
