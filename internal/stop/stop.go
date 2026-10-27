// Package stop carries the outcomes that end a run early: stop and ask the
// operator, or wait for something outside the run.
package stop

import (
	"errors"
	"fmt"
)

// Error ends a run early with a reason the ledger records.
type Error struct {
	// Reason is a ledger reason, such as GitChanged.
	Reason string
	// Message explains it to the operator.
	Message string
	// Wait is true when the run waits for something outside it, false
	// when it stops for the operator.
	Wait bool
}

func (e *Error) Error() string { return e.Reason + ": " + e.Message }

// Ask stops the run for the operator.
func Ask(reason, format string, a ...any) error {
	return &Error{Reason: reason, Message: fmt.Sprintf(format, a...)}
}

// Wait pauses the run until something outside it changes.
func Wait(reason, format string, a ...any) error {
	return &Error{Reason: reason, Message: fmt.Sprintf(format, a...), Wait: true}
}

// As returns the Error in err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
