// Command tack switches a stateful workload's primary between sites.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

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

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, factory{}, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
