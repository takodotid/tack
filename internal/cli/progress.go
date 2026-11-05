package cli

import (
	"bytes"
	"io"
	"sync"
	"time"

	"github.com/takodotid/tack/internal/engine"
)

// stamped writes progress lines: it starts each line with the time, in
// UTC, to the second ([RFC#0009] Requirement 27).
type stamped struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
	// mid is set while a line has started and not ended.
	mid bool
}

var _ engine.StampWriter = (*stamped)(nil)

// progress returns the writer of the progress lines that go to w: the
// engine's, the site checks' and Flux's. The plan, the status and the
// prompts go to w as they are.
func (o *options) progress(w io.Writer) io.Writer {
	return &stamped{w: w, now: o.now}
}

func (s *stamped) Write(p []byte) (int, error) {
	return s.write(p, time.Time{})
}

// WriteStamped implements engine.StampWriter: it starts each line of p with
// at, the time a site's check printed it, instead of the time it goes out.
func (s *stamped) WriteStamped(p []byte, at time.Time) (int, error) {
	return s.write(p, at)
}

// write starts each line with at, or with the time now when at is zero.
func (s *stamped) write(p []byte, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.IsZero() {
		at = s.now()
	}
	n := len(p)
	var b []byte
	for len(p) > 0 {
		if !s.mid {
			b = at.UTC().AppendFormat(b, time.RFC3339)
			b = append(b, ' ')
		}
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			b = append(b, p...)
			s.mid = true
			break
		}
		b = append(b, p[:i+1]...)
		p = p[i+1:]
		s.mid = false
	}
	if _, err := s.w.Write(b); err != nil {
		return 0, err
	}
	return n, nil
}
