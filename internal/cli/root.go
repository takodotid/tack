// Package cli defines the tack command line. The same definition produces
// the help, the man pages and the skill document for AI agents.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/kube"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitUsage   = 2
	ExitStopped = 3
	ExitWaiting = 4
)

// Version is set at build time.
var Version = "dev"

// Factory builds what a command needs. The binary supplies the real one;
// tests supply fakes.
type Factory interface {
	// Clients returns clients for the given site contexts.
	Clients(kubeconfig string, contexts map[string]string) kube.Clients
	// Env returns an Env for a workload, with its ledger at ledgerSite.
	// Close releases what it holds.
	Env(ctx context.Context, kubeconfig string, spec *v1alpha1.WorkloadSpec, ledgerSite string, out io.Writer) (env *engine.Env, close func(), err error)
}

type options struct {
	factory    Factory
	kubeconfig string
	skill      bool
	now        func() time.Time
	stdin      io.Reader
	isTerminal func() bool
}

const rootLong = `Tack switches a stateful workload's primary from one site to another: a
planned switchover, or a failover after a site is lost. It acts through the
workload's own API, keeps GitOps in step, and records every step in a
SiteFailover ledger in the new primary's cluster.

A run takes four phases, each step recorded before it starts and after it ends:

  Hold             annotate only the objects the run changes with
                   kustomize.toolkit.fluxcd.io/reconcile: disabled, so Flux
                   stops applying them
  Failover         change the live objects through the workload's API, in
                   order, waiting for each condition
  PromoteToGitOps  commit to Git the values that are now live
  Release          check that Git equals live, remove the annotations, and
                   check that Flux changed nothing

The operator starts every run. Tack never decides to fail over by itself.

Rules for AI agents:
  - An agent never starts "tack run" or "tack resume" without an explicit
    instruction from a human for that run.
  - "tack plan" comes first. Show its output to the human before any run.
  - An agent never passes --yes, --fence, --accept-git-head or --take-over
    unless the human's instruction for that run names it.
  - "tack plan" and "tack status" only read; an agent may run them freely.
  - When a run stops, report its reason and message to the human and wait.

Exit codes:
  0  success
  1  error
  2  usage error
  3  the run stopped and waits for the operator (see tack status)
  4  the run waits for something outside it, such as Git (tack resume later)`

// Hooks that tests replace.
var (
	nowFunc              = time.Now
	stdin      io.Reader = os.Stdin
	isTerminal           = stdinIsTerminal
)

// NewRoot returns the tack command.
func NewRoot(f Factory) *cobra.Command {
	o := &options{factory: f, now: nowFunc, stdin: stdin, isTerminal: isTerminal}
	root := &cobra.Command{
		Use:           "tack",
		Short:         "Switch a stateful workload's primary between sites, with GitOps kept in step",
		Long:          rootLong,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.skill {
				return writeSkill(cmd.OutOrStdout(), cmd)
			}
			return cmd.Help()
		},
	}
	root.PersistentFlags().StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig file holding every site's context (default: the standard kubeconfig loading rules)")
	root.Flags().BoolVar(&o.skill, "skill", false, "print a guide for AI agents as a skill document and exit")
	root.AddCommand(newPlan(o), newRun(o), newResume(o), newStatus(o))
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.CompletionOptions.DisableDefaultCmd = true
	return root
}

// Execute runs the command line and returns the exit code.
func Execute(f Factory, args []string, stdout, stderr io.Writer) int {
	root := NewRoot(f)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.Execute()
	if err == nil {
		return ExitOK
	}
	fmt.Fprintf(stderr, "tack: %v\n", err)
	var u usageError
	switch {
	case errors.As(err, &u):
		return ExitUsage
	case errors.Is(err, engine.ErrStopped):
		return ExitStopped
	case errors.Is(err, engine.ErrWaiting):
		return ExitWaiting
	}
	return ExitError
}

type usageError struct{ error }

func usagef(format string, a ...any) error { return usageError{fmt.Errorf(format, a...)} }
