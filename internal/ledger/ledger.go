// Package ledger stores SiteFailover objects in one site's cluster.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// Create implements engine.Ledger. It fails when the name is taken, and
// when the cluster's CRD would drop a field of the spec: a CRD older than
// this Tack prunes the fields it does not know, without an error. A dry
// run checks that before the ledger exists.
func (s *Store) Create(ctx context.Context, sf *failoverv1.SiteFailover) error {
	sf.Namespace = s.Namespace
	dry := sf.DeepCopy()
	if err := s.Client.Create(ctx, dry, client.DryRunAll); err != nil {
		return served(err)
	}
	if !equality.Semantic.DeepEqual(dry.Spec, sf.Spec) {
		return fmt.Errorf("the cluster's SiteFailover CRD drops fields of the ledger's spec: apply the CRD of this Tack version there; nothing was created")
	}
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

// EventSource is the component that the ledger's Events name.
const EventSource = "tack"

// Event implements engine.Recorder: it records a Kubernetes Event on the
// ledger sf, at time at, with a name the API server generates. A Warning
// when warning is set, else Normal.
func (s *Store) Event(ctx context.Context, sf *failoverv1.SiteFailover, warning bool, reason, message string, at time.Time) error {
	t := metav1.NewTime(at)
	typ := corev1.EventTypeNormal
	if warning {
		typ = corev1.EventTypeWarning
	}
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: sf.Namespace, GenerateName: sf.Name + "."},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: failoverv1.GroupVersion.String(), Kind: "SiteFailover",
			Namespace: sf.Namespace, Name: sf.Name, UID: sf.UID,
		},
		Reason: reason, Message: message, Type: typ,
		FirstTimestamp: t, LastTimestamp: t, Count: 1,
		Source: corev1.EventSource{Component: EventSource},
	}
	return s.Client.Create(ctx, ev)
}

// User implements engine.Recorder: the user name that the site's API
// server gives the client's credentials, from a SelfSubjectReview. The
// review is not stored.
func (s *Store) User(ctx context.Context) (string, error) {
	r := &authenticationv1.SelfSubjectReview{}
	if err := s.Client.Create(ctx, r); err != nil {
		return "", err
	}
	if r.Status.UserInfo.Username == "" {
		return "", errors.New("its SelfSubjectReview names no user")
	}
	return r.Status.UserInfo.Username, nil
}

// served explains an error from a cluster whose CRD does not serve this
// version, such as one still on the CRD of v0.1.x.
func served(err error) error {
	if meta.IsNoMatchError(err) {
		return fmt.Errorf("the cluster does not serve SiteFailover %s: install or upgrade Tack's CRD there: %w", failoverv1.GroupVersion, err)
	}
	return err
}
