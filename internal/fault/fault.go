// Package fault interrupts a run at a chosen step, for end-to-end tests.
// Only a binary built with the e2efault tag can be interrupted.
package fault

// Env names the variable that selects the interruption, as
// <step-id>:<before|after>. "before" exits after the ledger records the
// step as started; "after" exits after the step's action, before the ledger
// records its result.
const Env = "TACK_FAULT"

// ExitCode is the exit code of an interrupted run.
const ExitCode = 42
