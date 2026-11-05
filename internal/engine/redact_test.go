package engine_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/testenv"
)

// secretURL is a URL with a user and password, as a GitRepository's or a
// Kustomization's status message may carry one.
const secretURL = "https://user:tok3n-secret@git.example.invalid/gitops.git"

// urlFlux is the fake Flux, whose messages carry secretURL: a note of each
// reconcile, as a GitRepository that could not fetch gives, and an error
// of the check after release, as a Kustomization not Ready gives.
type urlFlux struct{ *testenv.Flux }

func (f urlFlux) Reconcile(ctx context.Context, site string, ks []failoverv1.ObjectKey, rev string, fetch bool, timeout time.Duration) (string, []string, error) {
	applied, notes, err := f.Flux.Reconcile(ctx, site, ks, rev, fetch, timeout)
	return applied, append(notes, "GitRepository flux-system/"+site+" did not fetch (GitOperationFailed: unable to clone '"+secretURL+"')"), err
}

func (f urlFlux) VerifyNoop(context.Context, string, []failoverv1.HeldObject, []failoverv1.ObjectField, bool, time.Duration) (string, error) {
	return "", errors.New("Kustomization flux-system/a is not Ready: kustomize build failed: fetch " + secretURL + ": 403")
}

// #35 row 43. Flux's and Strait's messages went into the ledger and the
// output without redaction. Every ledger write and progress line now
// passes through redact.Text ([RFC#0009] Requirement 10).
func TestOutsideMessagesAreRedacted(t *testing.T) {
	f := newFixture(t)
	var out bytes.Buffer
	env := f.env("b")
	env.Flux, env.Out = urlFlux{f.flux}, &out
	err := env.Start(context.Background(), f.plan("b", failoverv1.ModePlanned), engine.Options{})
	if !errors.Is(err, engine.ErrStopped) {
		t.Fatalf("run: %v, want a stop at the check after release", err)
	}
	sf := f.ledger("b")
	if stepState(sf, "verify-noop-a") != failoverv1.StepFailed {
		t.Fatalf("verify-noop-a %s, want Failed", stepState(sf, "verify-noop-a"))
	}
	var msgs []string
	msgs = append(msgs, sf.Status.Message, err.Error(), out.String())
	for _, st := range sf.Status.Steps {
		msgs = append(msgs, st.Message)
	}
	for _, h := range sf.Status.History {
		msgs = append(msgs, h.Message)
	}
	msgs = append(msgs, testenv.Events(f.clients.Sites["b"], "tack-system", "run-1")...)
	all := strings.Join(msgs, "\n")
	if strings.Contains(all, "tok3n-secret") || strings.Contains(all, "user:") {
		t.Errorf("a password is in the ledger or the output:\n%s", all)
	}
	for _, want := range []string{"step verify-noop-a: Kustomization flux-system/a is not Ready", "unable to clone 'https://***@git.example.invalid/gitops.git'",
		"Warning RunStopped: StepFailed: step verify-noop-a: Kustomization flux-system/a is not Ready: kustomize build failed: fetch https://***@git.example.invalid/gitops.git"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in the ledger or the output:\n%s", want, all)
		}
	}
}
