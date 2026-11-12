package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"sigs.k8s.io/yaml"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/config"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/version"
)

type planFlags struct {
	file   string
	from   string
	to     string
	mode   string
	output string
}

func (p *planFlags) add(cmd *cobra.Command, output bool) {
	cmd.Flags().StringVarP(&p.file, "file", "f", "", "the Workload file (required)")
	cmd.Flags().StringVar(&p.to, "to", "", "the site that becomes primary (required)")
	cmd.Flags().StringVar(&p.from, "from", "", "the site of the current primary (default: the primary the target site declares)")
	cmd.Flags().StringVar(&p.mode, "mode", "planned", "planned: the old primary demotes and hands over its token; site-loss: promote without a token after the old site is lost")
	if output {
		cmd.Flags().StringVarP(&p.output, "output", "o", "text", "text, or yaml for the SiteFailover the run would create")
	}
}

func (p *planFlags) request(w *config.Workload, now func() string) (engine.Request, error) {
	if p.file == "" || p.to == "" {
		return engine.Request{}, usagef("--file and --to are required")
	}
	var mode failoverv1.Mode
	switch p.mode {
	case "planned":
		mode = failoverv1.ModePlanned
	case "site-loss":
		mode = failoverv1.ModeSiteLoss
	default:
		return engine.Request{}, usagef("--mode must be planned or site-loss, not %q", p.mode)
	}
	return engine.Request{
		Run:      w.Name + "-" + now(),
		Workload: w.Name,
		Spec:     &w.Spec,
		Request:  driver.Request{From: p.from, To: p.to, Mode: mode},
	}, nil
}

const planLong = `Plan prints every step and every change a run would make, in order, and
changes nothing. It reads each reachable site and the Git repository.

The plan names the objects Tack holds, the patches and waits of the
workload's driver, the exact Git change (file, path and value), the checks
before and after release, and the follow-ups left for the operator. Values
captured during the run, such as a demotion token, appear as $(name).

Run "tack plan" before every "tack run", and show its output to the human
who decides.`

func newPlan(o *options) *cobra.Command {
	var p planFlags
	cmd := &cobra.Command{
		Use:   "plan -f WORKLOAD --to SITE [--mode planned|site-loss]",
		Short: "Print every step and change of a run, and change nothing",
		Long:  planLong,
		Example: `  # A planned switchover to site b
  tack plan -f workload.yaml --to b

  # A failover after site a is lost
  tack plan -f workload.yaml --to b --mode site-loss

  # The SiteFailover object the run would create
  tack plan -f workload.yaml --to b -o yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if p.output != "text" && p.output != "yaml" {
				return usagef("--output must be text or yaml")
			}
			plan, env, cleanup, err := o.makePlan(cmd.Context(), &p, o.progress(cmd.ErrOrStderr()))
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if p.output == "yaml" {
				b, err := yaml.Marshal(redacted(plan.SiteFailover))
				if err != nil {
					return err
				}
				_, err = out.Write(b)
				return err
			}
			printPlan(out, plan, env.Clients)
			fmt.Fprintf(out, "\nNothing was changed. To run this plan: tack run -f %s --to %s --from %s --mode %s\n",
				p.file, plan.SiteFailover.Spec.To, plan.SiteFailover.Spec.From, p.mode)
			return nil
		},
	}
	p.add(cmd, true)
	return cmd
}

// makePlan loads the Workload, fills --from and composes the plan.
func (o *options) makePlan(ctx context.Context, p *planFlags, progress io.Writer) (*engine.Plan, *engine.Env, func(), error) {
	if p.file == "" || p.to == "" {
		return nil, nil, nil, usagef("--file and --to are required")
	}
	w, err := o.loadWorkload(p.file)
	if err != nil {
		return nil, nil, nil, err
	}
	req, err := p.request(w, func() string { return o.now().UTC().Format("20060102-150405") })
	if err != nil {
		return nil, nil, nil, err
	}
	env, closeEnv, err := o.factory.Env(ctx, o.kubeconfig, &w.Spec, p.to, progress)
	if err != nil {
		return nil, nil, nil, err
	}
	env.Runs = o.workloadRuns(&w.Spec, w.Name, progress)
	plan, err := env.MakePlan(ctx, req)
	if errors.Is(err, engine.ErrNoFrom) {
		err = usageError{err}
	}
	return plan, env, closeEnv, err
}

func runLong() string {
	return `Run starts a run: it prints the plan, asks for confirmation, creates the
SiteFailover ledger at the target site, and takes the steps in order.

Modes:
  planned    the old primary demotes; its demotion token promotes the new one
  site-loss  the new site promotes without a token; the old primary then
             needs a rebuild. Use it only when the old site is lost:
             Tack cannot tell a lost site from one cut off from this
             host. Confirm the loss and fence the old primary first.

Confirming a site loss and fencing the old primary:
  ` + version.DocLink("docs/runbooks/site-loss.md") + `

If Git cannot be reached (break-glass), the run still holds and switches the
live objects, then waits with exit code 4; "tack resume" finishes it once Git
is back. If anything unexpected happens, the run stops with exit code 3 and
the ledger says why.

An agent never starts "tack run" without an explicit instruction from a
human for that run, and runs "tack plan" first.`
}

func newRun(o *options) *cobra.Command {
	var p planFlags
	var yes, force bool
	var takeOver []string
	cmd := &cobra.Command{
		Use:   "run -f WORKLOAD --to SITE [--mode planned|site-loss]",
		Short: "Start a run: hold Flux, switch the primary, commit to Git, release",
		Long:  runLong(),
		Example: `  # A planned switchover to site b, after reviewing "tack plan"
  tack run -f workload.yaml --to b

  # A failover after site a is lost
  tack run -f workload.yaml --to b --mode site-loss`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if force && len(takeOver) == 0 {
				return usagef("--force goes with --take-over: it takes over a run that a tack process may still run")
			}
			out := cmd.OutOrStdout()
			plan, env, cleanup, err := o.makePlan(cmd.Context(), &p, o.progress(out))
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				return err
			}
			printPlan(out, plan, env.Clients)
			if err := o.confirm(out, yes, plan.SiteFailover.Spec.Workload.Name); err != nil {
				return err
			}
			if err := env.Recheck(cmd.Context(), plan); err != nil {
				return err
			}
			old, err := o.checkNoActiveRun(cmd.Context(), env, plan.SiteFailover, takeOver, force)
			if err != nil {
				return err
			}
			if len(old) > 0 {
				env.Supersede = supersede(env, old)
			}
			return env.Start(cmd.Context(), plan.SiteFailover, engine.Options{TakeOver: takeOver, Yes: yes, Force: force})
		},
	}
	p.add(cmd, false)
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation; only when a human approved this run")
	cmd.Flags().StringSliceVar(&takeOver, "take-over", nil, "take over the holds of this earlier run, which can no longer finish; repeat it for each run")
	cmd.Flags().BoolVar(&force, "force", false, "with --take-over: "+forceHelp)
	return cmd
}

// confirm asks the operator to type the workload's name.
func (o *options) confirm(out io.Writer, yes bool, workload string) error {
	return o.confirmThat(out, yes, workload, "This changes live clusters and Git.", "this run")
}

// confirmThat asks the operator to type the workload's name after what,
// which says what the command changes. approved names what a human
// approves with --yes.
func (o *options) confirmThat(out io.Writer, yes bool, workload, what, approved string) error {
	if yes {
		return nil
	}
	if !o.isTerminal() {
		return usagef("no terminal to confirm on: pass --yes once a human approved %s", approved)
	}
	fmt.Fprintf(out, "\n%s Type the workload name %q to go on: ", what, workload)
	line, err := bufio.NewReader(o.stdin).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("no confirmation: %w", err)
	}
	if strings.TrimSpace(line) != workload {
		return fmt.Errorf("not confirmed; nothing was changed")
	}
	return nil
}

func stdinIsTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// checkNoActiveRun refuses a second active run of the same workload. It
// checks the sites again: the plan's checks may be older than the
// operator's confirmation. A run that another run took over is not active.
// It names each site it leaves out on the progress lines, and returns the
// runs to take over that it found. It refuses to take over a run that a
// tack process may still run, unless force ([RFC#0003] Requirement 28).
func (o *options) checkNoActiveRun(ctx context.Context, env *engine.Env, sf *failoverv1.SiteFailover, takeOver []string, force bool) ([]foundRun, error) {
	// Start searches again with the same checks once its ledger exists.
	env.StartChecks = env.SiteChecks(sf.Spec.Mode, sf.Spec.To)
	found, err := findRuns(ctx, env.StartChecks, &sf.Spec.Workload.Spec, sf.Spec.Workload.Name)
	if err != nil {
		return nil, err
	}
	o.leftOut(env.Out, found.missing)
	all := make([]failoverv1.SiteFailover, len(found.runs))
	for i, r := range found.runs {
		all[i] = *r.sf
	}
	var old []foundRun
	for _, r := range found.runs {
		if slices.Contains(takeOver, r.sf.Name) {
			cl, err := env.StartChecks.Client(r.site)
			if err != nil {
				return nil, err
			}
			if err := o.checkLive(ctx, env.Out, &ledger.Store{Client: cl, Namespace: r.sf.Namespace}, r.sf, force); err != nil {
				return nil, fmt.Errorf("--take-over %s, at site %s: %w", r.sf.Name, r.site, err)
			}
			old = append(old, r)
			continue
		}
		if engine.Ended(r.sf, all) {
			continue
		}
		if r.sf.Status.State == "" {
			return nil, fmt.Errorf("run %s at site %s never recorded its start, so none of its steps ran: take it over (--take-over %s)",
				r.sf.Name, r.site, r.sf.Name)
		}
		return nil, fmt.Errorf("run %s at site %s is %s: resume it (tack resume %s) or take it over (--take-over %s)",
			r.sf.Name, r.site, orPendingRun(r.sf.Status.State), r.sf.Name, r.sf.Name)
	}
	return old, nil
}

// supersede returns the Supersede of a run that takes over the runs old:
// it writes the new run's name into the status of the run named, at its
// site, through env's clients, which found it, with an entry TakenOver in
// its history and the entry's Event.
func supersede(env *engine.Env, old []foundRun) func(ctx context.Context, run, by string) error {
	return func(ctx context.Context, run, by string) error {
		i := slices.IndexFunc(old, func(r foundRun) bool { return r.sf.Name == run })
		if i < 0 {
			return fmt.Errorf("run %s was not found", run)
		}
		old := old[i]
		cl, err := env.Clients.Client(old.site)
		if err != nil {
			return err
		}
		store := &ledger.Store{Client: cl, Namespace: old.sf.Namespace}
		sf, err := store.Get(ctx, run)
		if err != nil {
			return err
		}
		if err := engine.CheckWriter(sf); err != nil {
			return err
		}
		sf.Status.SupersededBy = by
		entry := env.Entry(ctx, sf, failoverv1.HistoryTakenOver, []string{"--take-over=" + run})
		entry.Reason, entry.Message = "", "run "+by+" took this run over"
		engine.AppendHistory(&sf.Status, entry)
		engine.Stamp(&sf.Status)
		if err := store.UpdateStatus(ctx, sf); err != nil {
			return err
		}
		env.EventOn(ctx, store, sf, false, "Run"+string(failoverv1.HistoryTakenOver), engine.EntryText(sf.Status.History[len(sf.Status.History)-1]))
		return nil
	}
}
