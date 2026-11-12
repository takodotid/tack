package ledger

import (
	"context"

	coordinationv1 "k8s.io/api/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/takodotid/tack/internal/kube"
)

// Lease implements engine.Leaser: the Lease named after run in the
// store's namespace ([RFC#0003] Requirement 28).
func (s *Store) Lease(ctx context.Context, run string) (*coordinationv1.Lease, error) {
	l := &coordinationv1.Lease{}
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: run}, l); err != nil {
		return nil, err
	}
	return l, nil
}

// CreateLease implements engine.Leaser.
func (s *Store) CreateLease(ctx context.Context, l *coordinationv1.Lease) error {
	l.Namespace = s.Namespace
	return s.Client.Create(ctx, l, client.FieldOwner(kube.FieldManager))
}

// UpdateLease implements engine.Leaser. The Lease's resourceVersion makes
// the write fail when another writer changed it since it was read.
func (s *Store) UpdateLease(ctx context.Context, l *coordinationv1.Lease) error {
	return s.Client.Update(ctx, l, client.FieldOwner(kube.FieldManager))
}
