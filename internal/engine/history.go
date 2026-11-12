package engine

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/version"
)

// EventTimeout bounds the write of one Event. A failed Event is not
// repeated: the ledger's history is the record ([RFC#0003] Requirement 23).
const EventTimeout = 5 * time.Second

// userTimeout bounds the read of the user who runs this process.
const userTimeout = 5 * time.Second

// process is what one tack process records of itself in a ledger.
type process struct {
	userRead bool
	user     string
	// eventsOff is set after an Event failed: the process sends no more,
	// so a site that does not take Events slows no step.
	eventsOff bool
}

// Flags returns the operator's decision flags in opts, as a history entry
// records them.
func (o Options) Flags() []string {
	var out []string
	for _, r := range o.TakeOver {
		out = append(out, "--take-over="+r)
	}
	for _, s := range o.Fence {
		out = append(out, "--fence="+s)
	}
	if o.AcceptGitHead != "" {
		out = append(out, "--accept-git-head="+o.AcceptGitHead)
	}
	if o.Yes {
		out = append(out, "--yes")
	}
	if o.Force {
		out = append(out, "--force")
	}
	return out
}

// user returns who runs this process, as the API server of the ledger's
// site names the process's credentials. It reads it once per process, in
// a read that an interrupt does not cancel. "" when the site did not say;
// a progress line then says why.
func (e *Env) user(ctx context.Context) string {
	p := &e.proc
	if p.userRead {
		return p.user
	}
	p.userRead = true
	rec, ok := e.Ledger.(Recorder)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), userTimeout)
	defer cancel()
	u, err := rec.User(ctx)
	if err != nil {
		e.printf("  note      the ledger's site did not say who runs this tack (%v): its history entries name no user\n", err)
		return ""
	}
	p.user = u
	return u
}

// Entry returns the history entry of a status write by this process that
// leaves sf in state: the run's reason and message, this tack's version,
// the user who runs it, and flags.
func (e *Env) Entry(ctx context.Context, sf *failoverv1.SiteFailover, state failoverv1.HistoryState, flags []string) failoverv1.HistoryEntry {
	return failoverv1.HistoryEntry{
		Time: metav1.NewTime(e.now()), State: state, Reason: sf.Status.Reason, Message: sf.Status.Message,
		Version: version.Version, User: e.user(ctx), Flags: flags,
	}
}

// AppendHistory appends entry to the status's history ([RFC#0003]
// Requirement 22). At MaxHistory entries, it first drops the oldest entry
// but the first, which records the run's start, and counts it in
// historyDropped. Entries are never changed.
func AppendHistory(st *failoverv1.SiteFailoverStatus, entry failoverv1.HistoryEntry) {
	for len(st.History) >= failoverv1.MaxHistory {
		st.History = append(st.History[:1], st.History[2:]...)
		st.HistoryDropped++
	}
	st.History = append(st.History, entry)
}

// cut returns s cut to at most n bytes, at a rune boundary, and marked.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = " [cut]"
	end := n - len(mark)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + mark
}

// event records a Kubernetes Event on the ledger sf at the ledger's site,
// as EventOn does.
func (e *Env) event(ctx context.Context, sf *failoverv1.SiteFailover, warning bool, reason, message string) {
	if rec, ok := e.Ledger.(Recorder); ok {
		e.EventOn(ctx, rec, sf, warning, reason, message)
	}
}

// EventOn records a Kubernetes Event on the ledger sf through rec, after
// the ledger write it reports ([RFC#0003] Requirement 23). The message is
// cleaned like the ledger's, and cut to MaxHistoryMessage bytes. An Event
// is best effort: it is not repeated, and once one fails, the process
// sends no more, says so once, and goes on.
func (e *Env) EventOn(ctx context.Context, rec Recorder, sf *failoverv1.SiteFailover, warning bool, reason, message string) {
	if e.proc.eventsOff {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), EventTimeout)
	defer cancel()
	msg := cut(Clean(sf.Status.Captured, message), failoverv1.MaxHistoryMessage)
	if err := rec.Event(ctx, sf, warning, reason, msg, e.now()); err != nil {
		e.proc.eventsOff = true
		e.printf("  note      the ledger's site did not take an Event on run %s (%v): this tack sends no more Events; the ledger's history is the record\n", sf.Name, err)
	}
}

// entryEvent records the Event of a history entry: reason Run<State>, a
// Warning for a stop, a wait and an abandonment.
func (e *Env) entryEvent(ctx context.Context, rec Recorder, sf *failoverv1.SiteFailover, h failoverv1.HistoryEntry) {
	warning := false
	switch h.State {
	case failoverv1.HistoryState(failoverv1.RunStopped), failoverv1.HistoryState(failoverv1.RunWaiting), failoverv1.HistoryAbandoned:
		warning = true
	}
	e.EventOn(ctx, rec, sf, warning, "Run"+string(h.State), EntryText(h))
}

// EntryText is a history entry on one line: its reason and message, then
// the version, the user and the flags of the tack that recorded it.
func EntryText(h failoverv1.HistoryEntry) string {
	var b strings.Builder
	if h.Reason != "" {
		b.WriteString(h.Reason)
		if h.Message != "" {
			b.WriteString(": ")
		}
	}
	b.WriteString(h.Message)
	if b.Len() > 0 {
		b.WriteString(" ")
	}
	user := h.User
	if user == "" {
		user = "unknown"
	}
	fmt.Fprintf(&b, "(tack %s, user %s", orUnknown(h.Version), user)
	if len(h.Flags) > 0 {
		fmt.Fprintf(&b, ", %s", strings.Join(h.Flags, " "))
	}
	b.WriteString(")")
	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
