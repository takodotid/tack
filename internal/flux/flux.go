// Package flux holds and releases the objects that a Flux Kustomization
// applies, asks Flux to reconcile, and checks Git against live as
// kustomize-controller renders it.
package flux

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/stop"
)

const (
	// AnnotationHeldBy names the run that holds an object.
	AnnotationHeldBy = "failover.tako.id/held-by"
	// AnnotationReconcile set to ReconcileDisabled makes kustomize-controller
	// skip an object: no apply, no drift correction, no prune.
	AnnotationReconcile = "kustomize.toolkit.fluxcd.io/reconcile"
	// ReconcileDisabled is the value of AnnotationReconcile that holds an object.
	ReconcileDisabled = "disabled"
	// AnnotationRequestedAt asks a Flux object to reconcile now.
	AnnotationRequestedAt = "reconcile.fluxcd.io/requestedAt"

	// LabelKustomizationName and LabelKustomizationNamespace name the
	// Kustomization that applies an object.
	LabelKustomizationName      = "kustomize.toolkit.fluxcd.io/name"
	LabelKustomizationNamespace = "kustomize.toolkit.fluxcd.io/namespace"
	// LabelHelmName marks an object that helm-controller applies.
	LabelHelmName = "helm.toolkit.fluxcd.io/name"

	// ControllerFieldManager is the field manager of kustomize-controller.
	ControllerFieldManager = "kustomize-controller"
)

// Flux API kinds Tack reads and annotates.
var (
	KustomizationGVK = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
	GitRepositoryGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"}
)

// Flux implements engine.Flux against kustomize-controller and
// source-controller. It reads every Flux object as unstructured.
type Flux struct {
	Clients kube.Clients
	// Poll is the interval between checks of a wait.
	Poll time.Duration
	// Now returns the time. Tests replace it.
	Now func() time.Time
	// Out receives notes the result cannot carry, such as a skipped
	// Kustomization. Nil discards them.
	Out io.Writer

	differ differ
}

var _ engine.Flux = (*Flux)(nil)

// New returns a Flux for the sites' clients.
func New(c kube.Clients) *Flux { return &Flux{Clients: c} }

func (f *Flux) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Flux) poll() time.Duration {
	if f.Poll > 0 {
		return f.Poll
	}
	return 2 * time.Second
}

func (f *Flux) diff() differ {
	if f.differ != nil {
		return f.differ
	}
	return ssaDiffer{}
}

func (f *Flux) note(format string, a ...any) {
	if f.Out != nil {
		fmt.Fprintf(f.Out, "  note      "+format+"\n", a...)
	}
}

func (f *Flux) sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(f.poll()):
		return nil
	}
}

// Owner implements engine.Flux. It reads the object's owner labels.
func (f *Flux) Owner(ctx context.Context, site string, obj failoverv1.ObjectRef) (engine.Owner, error) {
	c, err := f.Clients.Client(site)
	if err != nil {
		return engine.Owner{}, err
	}
	u, err := getObject(ctx, c, obj)
	if err != nil {
		return engine.Owner{}, err
	}
	return owner(u), nil
}

func owner(u *unstructured.Unstructured) engine.Owner {
	l := u.GetLabels()
	var o engine.Owner
	if name, ns := l[LabelKustomizationName], l[LabelKustomizationNamespace]; name != "" && ns != "" {
		o.Kustomization = &failoverv1.ObjectKey{Namespace: ns, Name: name}
	}
	o.Helm = l[LabelHelmName] != ""
	return o
}

// Hold implements engine.Flux. It writes AnnotationReconcile and
// AnnotationHeldBy with an update that fails if the object changed since
// it was read.
func (f *Flux) Hold(ctx context.Context, site string, obj failoverv1.ObjectRef, run, takeOver string) (failoverv1.ObjectKey, error) {
	if run == "" {
		return failoverv1.ObjectKey{}, fmt.Errorf("hold %s: no run name", refString(obj))
	}
	c, err := f.Clients.Client(site)
	if err != nil {
		return failoverv1.ObjectKey{}, err
	}
	for attempt := 0; ; attempt++ {
		u, err := getObject(ctx, c, obj)
		if err != nil {
			return failoverv1.ObjectKey{}, err
		}
		o := owner(u)
		if o.Kustomization == nil {
			if o.Helm {
				return failoverv1.ObjectKey{}, fmt.Errorf("%s at site %s is applied by helm-controller; Tack holds only objects that a Flux Kustomization applies", refString(obj), site)
			}
			return failoverv1.ObjectKey{}, fmt.Errorf("%s at site %s has no %s and %s labels: no Flux Kustomization applies it", refString(obj), site, LabelKustomizationName, LabelKustomizationNamespace)
		}
		ann := u.GetAnnotations()
		heldBy := ann[AnnotationHeldBy]
		switch {
		case heldBy != "" && heldBy != run && (takeOver == "" || heldBy != takeOver):
			return failoverv1.ObjectKey{}, stop.Ask(failoverv1.ReasonHeldByAnotherRun,
				"%s at site %s is held by run %s. Take it over only once that run is abandoned", refString(obj), site, heldBy)
		case disabledValue(u.GetLabels()[AnnotationReconcile]):
			return failoverv1.ObjectKey{}, stop.Ask(failoverv1.ReasonHeldByAnotherRun,
				"%s at site %s is held outside Tack: it has the label %s=%s. Tack never removes a hold it did not set",
				refString(obj), site, AnnotationReconcile, u.GetLabels()[AnnotationReconcile])
		case heldBy == "" && disabledValue(ann[AnnotationReconcile]):
			return failoverv1.ObjectKey{}, stop.Ask(failoverv1.ReasonHeldByAnotherRun,
				"%s at site %s is held outside Tack: it has %s=%s without %s. Tack never removes a hold it did not set",
				refString(obj), site, AnnotationReconcile, ann[AnnotationReconcile], AnnotationHeldBy)
		case heldBy == run && disabledValue(ann[AnnotationReconcile]):
			return *o.Kustomization, nil
		}
		if ann == nil {
			ann = map[string]string{}
		}
		ann[AnnotationReconcile] = ReconcileDisabled
		ann[AnnotationHeldBy] = run
		u.SetAnnotations(ann)
		err = c.Update(ctx, u, client.FieldOwner(engine.FieldManager))
		if err == nil {
			return *o.Kustomization, nil
		}
		if !apierrors.IsConflict(err) || attempt > 0 {
			return failoverv1.ObjectKey{}, fmt.Errorf("hold %s at site %s: %w", refString(obj), site, err)
		}
	}
}

// Release implements engine.Flux. It removes the hold only when run set it.
func (f *Flux) Release(ctx context.Context, site string, obj failoverv1.ObjectRef, run string) (int64, error) {
	if run == "" {
		return 0, fmt.Errorf("release %s: no run name", refString(obj))
	}
	c, err := f.Clients.Client(site)
	if err != nil {
		return 0, err
	}
	for attempt := 0; ; attempt++ {
		u, err := getObject(ctx, c, obj)
		if err != nil {
			return 0, err
		}
		ann := u.GetAnnotations()
		heldBy := ann[AnnotationHeldBy]
		switch {
		case disabledValue(u.GetLabels()[AnnotationReconcile]):
			return 0, stop.Ask(failoverv1.ReasonHeldByAnotherRun,
				"%s at site %s is held outside Tack: it has the label %s=%s. Tack never removes a hold it did not set",
				refString(obj), site, AnnotationReconcile, u.GetLabels()[AnnotationReconcile])
		case heldBy == run:
		case heldBy != "":
			return 0, stop.Ask(failoverv1.ReasonHeldByAnotherRun,
				"%s at site %s is held by run %s, not %s. Tack leaves it held", refString(obj), site, heldBy, run)
		case disabledValue(ann[AnnotationReconcile]):
			return 0, stop.Ask(failoverv1.ReasonHeldByAnotherRun,
				"%s at site %s is held outside Tack: it has %s=%s without %s. Tack never removes a hold it did not set",
				refString(obj), site, AnnotationReconcile, ann[AnnotationReconcile], AnnotationHeldBy)
		default:
			return u.GetGeneration(), nil
		}
		delete(ann, AnnotationHeldBy)
		delete(ann, AnnotationReconcile)
		u.SetAnnotations(ann)
		err = c.Update(ctx, u, client.FieldOwner(engine.FieldManager))
		if err == nil {
			return u.GetGeneration(), nil
		}
		if !apierrors.IsConflict(err) || attempt > 0 {
			return 0, fmt.Errorf("release %s at site %s: %w", refString(obj), site, err)
		}
	}
}

// disabledValue is true for the value that makes kustomize-controller skip
// an object. kustomize-controller compares it ignoring case.
func disabledValue(v string) bool { return strings.EqualFold(v, ReconcileDisabled) }

// isHeld is true when kustomize-controller skips the object.
func isHeld(u *unstructured.Unstructured) bool {
	return disabledValue(u.GetLabels()[AnnotationReconcile]) || disabledValue(u.GetAnnotations()[AnnotationReconcile])
}

func newObject(ref failoverv1.ObjectRef) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind))
	u.SetNamespace(ref.Namespace)
	u.SetName(ref.Name)
	return u
}

func getObject(ctx context.Context, c client.Client, ref failoverv1.ObjectRef) (*unstructured.Unstructured, error) {
	u := newObject(ref)
	if err := c.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
		return nil, fmt.Errorf("get %s: %w", refString(ref), err)
	}
	return u, nil
}

func getKind(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, key failoverv1.ObjectKey) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Name}, u); err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", gvk.Kind, key.Namespace, key.Name, err)
	}
	return u, nil
}

func refString(o failoverv1.ObjectRef) string {
	return fmt.Sprintf("%s %s/%s", o.Kind, o.Namespace, o.Name)
}

func keyString(k failoverv1.ObjectKey) string { return k.Namespace + "/" + k.Name }
