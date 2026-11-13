package engine

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
)

// [RFC#0003] Requirement 30: a step keeps its newest earlier attempts, at
// most MaxEarlierAttempts, each message cut to MaxHistoryMessage bytes.
func TestKeepAttemptIsBounded(t *testing.T) {
	st := &failoverv1.StepStatus{}
	keepAttempt(st)
	if len(st.Earlier) != 0 {
		t.Fatalf("a step that never ran keeps %v", st.Earlier)
	}
	for i := 1; i <= failoverv1.MaxEarlierAttempts+5; i++ {
		now := metav1.Now()
		st.StartedAt, st.State, st.Attempts = &now, failoverv1.StepFailed, int32(i)
		st.Message = fmt.Sprintf("attempt %d %s", i, strings.Repeat("x", 2*failoverv1.MaxHistoryMessage))
		keepAttempt(st)
	}
	if len(st.Earlier) != failoverv1.MaxEarlierAttempts {
		t.Fatalf("%d earlier attempts, want %d", len(st.Earlier), failoverv1.MaxEarlierAttempts)
	}
	if !strings.HasPrefix(st.Earlier[0].Message, "attempt 6 ") || !strings.HasPrefix(st.Earlier[len(st.Earlier)-1].Message, fmt.Sprintf("attempt %d ", failoverv1.MaxEarlierAttempts+5)) {
		t.Errorf("kept %q to %q, want the newest", st.Earlier[0].Message[:12], st.Earlier[len(st.Earlier)-1].Message[:12])
	}
	for _, a := range st.Earlier {
		if len(a.Message) > failoverv1.MaxHistoryMessage {
			t.Errorf("a message of %d bytes", len(a.Message))
		}
	}
}
