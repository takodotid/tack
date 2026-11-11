package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
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

Abandon changes nothing else. A hold the run left on an object stays:
remove the holds once Git equals live, or start the next run with
--take-over RUN, which takes them over. Abandon lists the holds as the run's
ledger records them, and does not read the objects: a hold removed by hand
is still listed. The runbook:
  ` + version.DocLink("docs/runbooks/abandon-run.md") + `

kubectl get sfo shows when the run was abandoned in its Abandoned column,
from the CRD of tack v1.4.0 on. Its State column keeps the run's last
state, such as Stopped. tack status lists the run as Abandoned.

Abandon does not stop a tack process that runs the run: check first that
none does. A step that runs, such as a promotion, still changes the
workload. Abandon refuses while the run's lease says that a tack process
runs it, or, for a run without a lease, while the run is Running and its
ledger was written within its step's timeout and 5 minutes; --force goes on
all the same. Only tack v1.3.0 and later honour the mark: an older tack
resumes the run, and drops the mark.

An agent never starts "tack abandon" without an explicit instruction from a
human for that run.`
}

// defaultAbandonReason is the reason tack abandon records without --reason.
const defaultAbandonReason = "abandoned by the operator with tack abandon"

func newAbandon(o *options) *cobra.Command {
	var l ledgerFlags
	var yes bool
	var reason string
	var force bool
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
			found, err := l.find(ctx, o, args[0], progress)
			if err != nil {
				return err
			}
			r, err := found.pick(args[0])
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
			if err := engine.CheckWriter(sf); err != nil {
				return err
			}
			// The list is the ledger's: abandon reads no object of the
			// workload, and a hold removed by hand stays in it (#59).
			held := stillHeld(sf)
			if len(held) > 0 {
				fmt.Fprintln(out, "The ledger records these objects as held by the run, and not released:")
				for _, h := range held {
					fmt.Fprintf(out, "  %s at site %s\n", ref(h.Object), h.Site)
				}
				fmt.Fprintf(out, "Abandon does not read the objects: a hold removed by hand is still listed. A hold still on an object stays after the run is abandoned: remove it once Git equals live (%s), or start the next run with --take-over %s.\n", version.DocLink("docs/runbooks/abandon-run.md"), sf.Name)
			}
			// A process that runs the run holds its lease (#65): abandon
			// checks it before it asks, and again after.
			store, _ := env.Ledger.(engine.Leaser)
			if err := o.checkLive(ctx, progress, store, sf, force); err != nil {
				return err
			}
			fmt.Fprintf(out, "Check that no tack process runs %s: abandon does not stop one. Only tack v1.3.0 and later honour the mark.\n", sf.Name)
			if err := o.confirmThat(out, yes, sf.Spec.Workload.Name, "This changes only the run's ledger, at site "+sf.Spec.To+".", "abandoning this run"); err != nil {
				return err
			}
			if err := o.checkLive(ctx, progress, store, sf, force); err != nil {
				return err
			}
			sf.Status.Abandoned = &failoverv1.Abandonment{At: metav1.NewTime(o.now()), Reason: reason}
			var flags []string
			if yes {
				flags = append(flags, "--yes")
			}
			if force {
				flags = append(flags, "--force")
			}
			entry := env.Entry(ctx, sf, failoverv1.HistoryAbandoned, flags)
			entry.Reason, entry.Message = "", reason
			engine.AppendHistory(&sf.Status, entry)
			engine.Stamp(&sf.Status)
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
			// The State column reads status.state, which the mark leaves
			// as it was; the Abandoned column reads the mark, at a site
			// whose CRD has it ([RFC#0003] Requirement 15).
			state := "stays empty"
			if s := back.Status.State; s != "" {
				state = "keeps " + string(s)
			}
			fmt.Fprintf(out, "kubectl get sfo shows the mark in its Abandoned column, with the CRD of tack v1.4.0 or later at the ledger's site; its State column %s (%s)\n", state, version.DocLink("docs/operations.md#reading-a-run"))
			return nil
		},
	}
	l.add(cmd)
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation; only when a human approved abandoning this run")
	cmd.Flags().StringVar(&reason, "reason", defaultAbandonReason, "why the run is abandoned, recorded in its ledger")
	cmd.Flags().BoolVar(&force, "force", false, forceHelp)
	return cmd
}

// forceHelp is the help of --force, on tack abandon and tack run.
const forceHelp = "go on although a tack process may still run the run: its lease is fresh, or its ledger was just written; only once a human checked that none does"

// checkLive refuses to mark or take over run sf while a tack process may
// still run it ([RFC#0003] Requirement 28), unless force, which goes on
// with a line that says so. store reads the run's lease at its ledger's
// site; nil judges by the ledger alone. It changes nothing.
func (o *options) checkLive(ctx context.Context, progress io.Writer, store engine.Leaser, sf *failoverv1.SiteFailover, force bool) error {
	note, err := engine.CheckLive(ctx, store, sf, o.now())
	if note != "" {
		fmt.Fprintf(progress, "  note      %s: the ledger tells instead\n", note)
	}
	switch {
	case err == nil:
		return nil
	case !force:
		return fmt.Errorf("%w. Nothing was changed", err)
	}
	fmt.Fprintf(progress, "  note      --force: going on, although %v\n", strings.TrimPrefix(err.Error(), engine.ErrMayRun.Error()+": "))
	return nil
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
