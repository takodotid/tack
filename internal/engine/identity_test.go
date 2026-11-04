package engine_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// otherCluster is a cluster that is not site a's, with the same Cluster
// and Pooler names, such as a staging copy. Its kube-system namespace has
// another UID.
func otherCluster() client.Client {
	ns := &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-other")}}
	return testenv.NewClient(append(testenv.Site("a", "a", 2), ns)...)
}

func resourceVersion(t *testing.T, c client.Client, kind, name string) string {
	t.Helper()
	u := testenv.Object("postgresql.cnpg.io/v1", kind, "db", name, nil)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(u), u); err != nil {
		t.Fatal(err)
	}
	return u.GetResourceVersion()
}

// Issue #32. A run stops at its first step; it is resumed with a
// kubeconfig whose context for site a reaches another cluster. The resume
// must stop before it acts at site a, and change nothing there.
func TestResumeStopsOnAnotherCluster(t *testing.T) {
	f := newFixture(t)
	f.flux.Fail["Hold"] = errors.New("stop at the first hold")
	if _, err := f.start("b", failoverv1.ModePlanned, engine.Options{}); !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run: %v", err)
	}
	delete(f.flux.Fail, "Hold")

	other := otherCluster()
	f.clients.SetSite("a", other)
	before := resourceVersion(t, other, "Cluster", "pg-a") + resourceVersion(t, other, "Pooler", "pg-a-pooler-rw")
	_, err := f.resume("b", engine.Options{})
	if err == nil {
		t.Error("the resume did not stop")
	}
	if after := resourceVersion(t, other, "Cluster", "pg-a") + resourceVersion(t, other, "Pooler", "pg-a-pooler-rw"); after != before {
		t.Errorf("the resume changed the other cluster: %s, then %s", before, after)
	}
	if n := holdsAt(f.flux, "a"); n != 1 {
		t.Errorf("%d holds at site a, want only the first, failed one", n)
	}
}

// The plan records the cluster of each site that answers. A site lost at
// plan time is recorded when a process first reads it, and checked after.
func TestFingerprints(t *testing.T) {
	f := newFixture(t)
	sf := f.plan("b", failoverv1.ModePlanned)
	want := []failoverv1.SiteFingerprint{{Site: "a", KubeSystemUID: "uid-a"}, {Site: "b", KubeSystemUID: "uid-b"}}
	if got := sf.Spec.Fingerprints; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("spec.fingerprints %v, want %v", got, want)
	}

	f = newFixture(t)
	f.clients.SetDown("a", true)
	sf, err := f.start("b", failoverv1.ModeSiteLoss, engine.Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sf.Spec.Fingerprints; len(got) != 1 || got[0].Site != "b" {
		t.Fatalf("spec.fingerprints %v, want site b only", got)
	}
	f.clients.SetDown("a", false)
	setPrimary(t, f, "a", "pg-b")
	sf, err = f.resume("b", engine.Options{})
	if err != nil || sf.Status.State != failoverv1.RunSucceeded {
		t.Fatalf("resume: %v %s", err, sf.Status.Message)
	}
	if uid, ok := engine.Fingerprint(sf, "a"); !ok || uid != "uid-a" {
		t.Fatalf("site a recorded as %q, %v; status.fingerprints %v", uid, ok, sf.Status.Fingerprints)
	}
}

// A ledger written by v1.1.x has no fingerprints. It resumes: each site is
// recorded on its first read, and checked after.
func TestResumeLedgerWithoutFingerprints(t *testing.T) {
	f := newFixture(t)
	sf := f.plan("b", failoverv1.ModePlanned)
	sf.Spec.Fingerprints = nil
	f.flux.Fail["VerifyGit"] = errors.New("stop before release")
	sf, err := f.run("b", sf, engine.Options{})
	if !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run: %v", err)
	}
	for _, site := range []string{"a", "b"} {
		if uid, ok := engine.Fingerprint(sf, site); !ok || uid != "uid-"+site {
			t.Errorf("site %s recorded as %q, %v", site, uid, ok)
		}
	}
	delete(f.flux.Fail, "VerifyGit")
	f.clients.SetSite("a", otherCluster())
	sf, err = f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonSiteMismatch {
		t.Fatalf("err %v, reason %s: %s", err, sf.Status.Reason, sf.Status.Message)
	}
	if st := stepState(sf, "verify-git-a"); st != failoverv1.StepFailed {
		t.Errorf("verify-git-a is %s, want Failed as the first process left it", st)
	}
}
