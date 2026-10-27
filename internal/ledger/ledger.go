// Package ledger stores SiteFailover objects in one site's cluster.
package ledger

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/api/v1alpha1"
)

// Store keeps the SiteFailover objects of one namespace at one site.
type Store struct {
	Client    client.Client
	Namespace string
	// Selector limits List, such as to one workload.
	Selector map[string]string
}

// Create implements engine.Ledger. It fails when the name is taken.
func (s *Store) Create(ctx context.Context, sf *v1alpha1.SiteFailover) error {
	sf.Namespace = s.Namespace
	return s.Client.Create(ctx, sf)
}

// Get implements engine.Ledger.
func (s *Store) Get(ctx context.Context, name string) (*v1alpha1.SiteFailover, error) {
	sf := &v1alpha1.SiteFailover{}
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, sf); err != nil {
		return nil, err
	}
	return sf, nil
}

// UpdateStatus implements engine.Ledger. The object's resourceVersion makes
// the write fail when another writer changed it since it was read.
func (s *Store) UpdateStatus(ctx context.Context, sf *v1alpha1.SiteFailover) error {
	if sf.ResourceVersion == "" {
		return fmt.Errorf("ledger %s has no resourceVersion", sf.Name)
	}
	return s.Client.Status().Update(ctx, sf)
}

// List implements engine.Ledger, newest first.
func (s *Store) List(ctx context.Context) ([]v1alpha1.SiteFailover, error) {
	var l v1alpha1.SiteFailoverList
	opts := []client.ListOption{client.InNamespace(s.Namespace)}
	if len(s.Selector) > 0 {
		opts = append(opts, client.MatchingLabels(s.Selector))
	}
	if err := s.Client.List(ctx, &l, opts...); err != nil {
		return nil, err
	}
	sort.Slice(l.Items, func(i, j int) bool {
		return l.Items[j].CreationTimestamp.Before(&l.Items[i].CreationTimestamp)
	})
	return l.Items, nil
}
