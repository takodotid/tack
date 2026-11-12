// Command tack switches a stateful workload's primary between sites.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/cli"
	"github.com/takodotid/tack/internal/driver"
	_ "github.com/takodotid/tack/internal/driver/cnpg"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/flux"
	"github.com/takodotid/tack/internal/gitops"
	"github.com/takodotid/tack/internal/kube"
	"github.com/takodotid/tack/internal/ledger"
	"github.com/takodotid/tack/internal/strait"
)

// controller-runtime's clients log through a logger of their own, which
// prints a warning with a stack trace on stderr when nothing set it within
// 30 s of the start, such as in a run that waits for a site. Tack keeps
// none of those logs.
func init() { ctrllog.SetLogger(logr.Discard()) }

type factory struct{}

func (factory) Clients(kubeconfig string, contexts map[string]string) kube.Clients {
	return kube.NewKubeconfig(kubeconfig, contexts)
}

func (f factory) Env(_ context.Context, kubeconfig string, spec *failoverv1.WorkloadSpec, ledgerSite string, out io.Writer) (*engine.Env, func(), error) {
	d, err := driver.Get(spec.Driver)
	if err != nil {
		return nil, nil, err
	}
	clients := kube.NewKubeconfig(kubeconfig, kube.Contexts(spec))
	dir, err := os.MkdirTemp("", "tack-")
	if err != nil {
		return nil, nil, err
	}
	git := gitops.New(dir)
	fl := flux.New(clients)
	fl.Out = out
	env := &engine.Env{
		Clients: clients, Driver: d, Flux: fl, Git: git, Routing: strait.New(clients), Out: out,
	}
	cl, err := clients.Client(ledgerSite)
	if err != nil {
		git.Close()
		return nil, nil, fmt.Errorf("ledger site %s: %w", ledgerSite, err)
	}
	env.Ledger = &ledger.Store{Client: cl, Namespace: spec.Ledger.Namespace}
	return env, func() { git.Close(); os.RemoveAll(dir) }, nil
}

// signalContext returns a context that the first SIGINT, SIGTERM or
// SIGHUP cancels: the run then records why it stopped, in a ledger write
// that outlives the cancel. The handler is released at once, so a second
// signal ends tack with the default action, also during that write
// (#35 row 38). SIGHUP comes from a terminal or SSH session that ended.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

func main() {
	ctx, stop := signalContext()
	code := cli.Execute(ctx, factory{}, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
