package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/version"
)

func abandonLong() string {
	return `Abandon marks a run that must never go on, such as one whose old primary
was promoted again by hand: it records status.abandoned in the run's
ledger. tack resume then refuses the run, and a new run of the workload no
longer waits for it.

Abandon changes nothing else. The objects the run holds stay held: remove
the holds once Git equals live, or start the next run with --take-over RUN,
which takes them over. The runbook:
  ` + version.DocLink("docs/runbooks/abandon-run.md") + `

Abandon does not stop a tack process that runs the run: check first that
none does. A step that runs, such as a promotion, still changes the
workload. Only tack v1.3.0 and later honour the mark: an older tack resumes
the run, and drops the mark.

An agent never starts "tack abandon" without an explicit instruction from a
human for that run.`
}

// defaultAbandonReason is the reason tack abandon records without --reason.
const defaultAbandonReason = "abandoned by the operator with tack abandon"

func newAbandon(o *options) *cobra.Command {
	var l ledgerFlags
	var yes bool
	var reason string
	cmd := &cobra.Command{
		Use:   "abandon RUN (-f WORKLOAD | --context CONTEXT)",
		Short: "Mark a run that must never go on",
		Long:  abandonLong(),
		Example: `  # After the old primary was promoted again by hand
  tack abandon pg-main-20261027-101500 -f workload.yaml --reason "a promoted again by hand"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			if reason == "" {
				return usagef("--reason must not be empty")
			}
			progress := o.progress(out)
			runs, err := l.find(ctx, o, args[0], progress)
			if err != nil {
				return err
			}
			r, err := pick(runs, args[0])
			if err != nil {
				return err
			}
			spec := &r.sf.Spec.Workload.Spec
			env, closeEnv, err := o.factory.Env(ctx, o.kubeconfig, spec, r.sf.Spec.To, progress)
			if err != nil {
				return err
			}
			defer closeEnv()
			sf, err := env.Ledger.Get(ctx, r.sf.Name)
			if err != nil {
				return err
			}
			printStatus(out, sf)
			fmt.Fprintln(out)
			if a := sf.Status.Abandoned; a != nil {
				fmt.Fprintf(out, "run %s was already abandoned at %s: %s\n", sf.Name, a.At.UTC().Format(time.RFC3339), a.Reason)
				return nil
			}
			if sf.Status.State == failoverv1.RunSucceeded && !hasDeferred(sf) {
				return fmt.Errorf("run %s succeeded, and no step of it is left: there is nothing to abandon", sf.Name)
			}
			held := stillHeld(sf)
			if len(held) > 0 {
				fmt.Fprintf(out, "The run's holds stay after it is abandoned:\n")
				for _, h := range held {
					fmt.Fprintf(out, "  %s at site %s\n", ref(h.Object), h.Site)
				}
				fmt.Fprintf(out, "Remove them once Git equals live (%s), or start the next run with --take-over %s.\n", version.DocLink("docs/runbooks/abandon-run.md"), sf.Name)
			}
			fmt.Fprintf(out, "Check that no tack process runs %s: abandon does not stop one. Only tack v1.3.0 and later honour the mark.\n", sf.Name)
			if err := o.confirm(out, yes, sf.Spec.Workload.Name); err != nil {
				return err
			}
			sf.Status.Abandoned = &failoverv1.Abandonment{At: metav1.NewTime(o.now()), Reason: reason}
			var flags []string
			if yes {
				flags = []string{"--yes"}
			}
			entry := env.Entry(ctx, sf, failoverv1.HistoryAbandoned, flags)
			entry.Reason, entry.Message = "", reason
			engine.AppendHistory(&sf.Status, entry)
			engine.CleanStatus(&sf.Status)
			if err := env.Ledger.UpdateStatus(ctx, sf); err != nil {
				if apierrors.IsConflict(err) {
					return fmt.Errorf("the ledger of run %s changed since it was read, so another tack process may be running it. Nothing was changed: check, then abandon it again: %w", sf.Name, err)
				}
				return fmt.Errorf("write the ledger of run %s: %w", sf.Name, err)
			}
			back, err := env.Ledger.Get(ctx, sf.Name)
			if err != nil {
				return fmt.Errorf("read the ledger of run %s back: %w", sf.Name, err)
			}
			if back.Status.Abandoned == nil {
				return errors.New("the ledger's site dropped status.abandoned, so its CRD is older than this tack: apply this version's CRD there, then abandon the run again")
			}
			if rec, ok := env.Ledger.(engine.Recorder); ok && len(back.Status.History) > 0 {
				env.EventOn(ctx, rec, back, true, "Run"+string(failoverv1.HistoryAbandoned), engine.EntryText(back.Status.History[len(back.Status.History)-1]))
			}
			fmt.Fprintf(out, "run %s abandoned: tack resume refuses it, and a new run no longer waits for it\n", sf.Name)
			return nil
		},
	}
	l.add(cmd)
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation; only when a human approved abandoning this run")
	cmd.Flags().StringVar(&reason, "reason", defaultAbandonReason, "why the run is abandoned, recorded in its ledger")
	return cmd
}

// hasDeferred tells whether a step of the run waits for its site.
func hasDeferred(sf *failoverv1.SiteFailover) bool {
	for _, st := range sf.Status.Steps {
		if st.State == failoverv1.StepDeferred {
			return true
		}
	}
	return false
}

// stillHeld returns the objects the run held and did not release.
func stillHeld(sf *failoverv1.SiteFailover) []failoverv1.HeldObject {
	var out []failoverv1.HeldObject
	for _, h := range sf.Status.Held {
		if h.ReleasedAt == nil {
			out = append(out, h)
		}
	}
	return out
}
