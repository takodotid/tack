package engine_test

import (
	"errors"
	"strings"
	"testing"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
)

// Issue #46, from PR #48. A planned run from a to b dies after
// capture-token, before promote-b. a is promoted again by hand, then
// demoted again by hand: it is a replica, with the token of its second
// demotion. The read of the other sites passes. A resume must not promote
// b with the token of the first demotion: it stops with TokenMissing
// before promote-b, which keeps its state, and prints neither token.
func TestResumeStopsWhenTokenChanged(t *testing.T) {
	f := newFixture(t)
	f.diesBeforePromotion(f.plan("b", failoverv1.ModePlanned))
	first := f.ledger("b").Status.Captured["demotionToken"]
	setPrimary(t, f, "a", "pg-a")
	setPrimary(t, f, "a", "pg-b")
	second, _ := field(f.cluster("a"), "status", "demotionToken").(string)
	if second == "" || second == first {
		t.Fatalf("site a's token is %q after its second demotion, first %q", second, first)
	}
	before := resourceVersion(t, f.clients.Sites["b"], "Cluster", "pg-b")

	sf, err := f.resume("b", engine.Options{})
	if !errors.Is(err, engine.ErrStopped) || sf.Status.Reason != failoverv1.ReasonTokenMissing {
		t.Fatalf("err %v, %s %s: %s", err, sf.Status.State, sf.Status.Reason, sf.Status.Message)
	}
	for _, want := range []string{"step promote-b: Cluster db/pg-a now has another value at {.status.demotionToken}", "captured as demotionToken", "docs/runbooks/abandon-run.md"} {
		if !strings.Contains(sf.Status.Message, want) {
			t.Errorf("message %q lacks %q", sf.Status.Message, want)
		}
	}
	for _, tok := range []string{first, second} {
		if strings.Contains(sf.Status.Message, tok) {
			t.Errorf("message %q prints a token", sf.Status.Message)
		}
	}
	if st := stepState(sf, "promote-b"); st != failoverv1.StepPending {
		t.Errorf("promote-b is %s, want Pending", st)
	}
	if after := resourceVersion(t, f.clients.Sites["b"], "Cluster", "pg-b"); after != before {
		t.Errorf("Cluster pg-b changed: resourceVersion %s, then %s", before, after)
	}
	if got := sf.Status.Captured["demotionToken"]; got != first {
		t.Error("the ledger's captured token changed")
	}
}

// The compare needs the old primary. With a down at the resume, the run
// promotes b with the token it captured, as option 1 of
// docs/runbooks/no-primary-after-demotion.md says, and the promotion's
// message says that the token was not compared.
func TestResumeWithFromDownDoesNotCompareToken(t *testing.T) {
	f := newFixture(t)
	f.diesBeforePromotion(f.plan("b", failoverv1.ModePlanned))
	f.clients.SetDown("a", true)
	sf, _ := f.resume("b", engine.Options{})
	if st := stepState(sf, "wait-promoted-b"); st != failoverv1.StepSucceeded {
		t.Fatalf("wait-promoted-b is %s: %s", st, sf.Status.Message)
	}
	if msg := stepMessage(sf, "promote-b"); !strings.Contains(msg, "site a gave no answer, so demotionToken was not compared with Cluster db/pg-a") {
		t.Errorf("promote-b's message %q", msg)
	}
}
