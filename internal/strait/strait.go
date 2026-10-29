// Package strait reads Strait's SiteService status to confirm that routing
// followed a switch. Tack only reads Strait's objects; it never writes them.
package strait

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/kube"
)

// APIVersion and Kind of Strait's SiteService.
const (
	APIVersion = "strait.tako.id/v1"
	Kind       = "SiteService"
)

// ReasonActive is the route reason when one writable member is routed.
const ReasonActive = "Active"

// Reader implements engine.Routing.
type Reader struct {
	Clients kube.Clients
	// Poll is the interval between reads.
	Poll time.Duration
	// Now returns the time. Tests replace it.
	Now func() time.Time
}

// New returns a Reader.
func New(c kube.Clients) *Reader { return &Reader{Clients: c, Poll: 2 * time.Second, Now: time.Now} }

// Wait waits until the SiteService routes to the expected member only.
func (r *Reader) Wait(ctx context.Context, site string, s *failoverv1.RoutingStep) (string, error) {
	c, err := r.Clients.Client(site)
	if err != nil {
		return "", err
	}
	deadline := r.Now().Add(s.Timeout.Duration)
	var last string
	for {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(APIVersion)
		u.SetKind(Kind)
		err := c.Get(ctx, client.ObjectKey{Namespace: s.Service.Namespace, Name: s.Service.Name}, u)
		switch {
		case meta.IsNoMatchError(err):
			return "", fmt.Errorf("site %s does not serve %s %s: is Strait v1.0.0 or later installed?", site, Kind, APIVersion)
		case err != nil:
			last = err.Error()
		default:
			ok, why := Routed(u.Object, s.Expected)
			if ok {
				return fmt.Sprintf("Strait routes %s/%s to %s", s.Service.Namespace, s.Service.Name, s.Expected), nil
			}
			last = why
		}
		if r.Now().After(deadline) {
			return "", fmt.Errorf("timed out after %s waiting for Strait at site %s: %s", s.Timeout.Duration, site, last)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(r.Poll):
		}
	}
}

// Routed reports whether a SiteService's status routes to the expected
// member only, for the SiteService's current generation.
func Routed(obj map[string]any, expected string) (bool, string) {
	gen, _, _ := unstructured.NestedInt64(obj, "metadata", "generation")
	observed, _, _ := unstructured.NestedInt64(obj, "status", "observedGeneration")
	if observed != gen {
		return false, fmt.Sprintf("status is for generation %d, the object is at %d", observed, gen)
	}
	members, _, _ := unstructured.NestedStringSlice(obj, "status", "route", "members")
	reason, _, _ := unstructured.NestedString(obj, "status", "route", "reason")
	if len(members) == 1 && members[0] == expected && reason == ReasonActive {
		return true, ""
	}
	msg, _, _ := unstructured.NestedString(obj, "status", "route", "message")
	return false, fmt.Sprintf("route is [%s], reason %q: %s", strings.Join(members, ", "), reason, msg)
}
