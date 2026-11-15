// package main implements the provider-opensearch provider.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/openeverest/openeverest/v2/provider-runtime/preflight"
	"github.com/openeverest/openeverest/v2/provider-runtime/reconciler"

	"github.com/openeverest/provider-opensearch/internal/provider"
)

// main is the entry point for the provider.
func main() {
	var serverPort int
	var metricsBindAddress string
	var admission bool
	var preflightSpec string

	flag.IntVar(&serverPort, "server-port", 8082, "The port for the provider HTTP server.")
	flag.StringVar(&metricsBindAddress, "metrics-bind-address", ":8081", "The address the metrics endpoint binds to. Use 0 to disable.")
	flag.BoolVar(&admission, "admission", true, "Serve the Instance admission webhook, with the serving certificate the chart mounts.")
	flag.StringVar(&preflightSpec, "preflight-spec", "", "Run the upgrade preflight against the target Provider spec at this path and exit. Used by the chart's pre-upgrade hook.")
	flag.Parse()

	l := ctrl.Log.WithName("setup")
	ctx := ctrl.SetupSignalHandler()

	p := provider.New()

	if preflightSpec != "" {
		if err := preflight.Run(ctx, p, preflight.Options{TargetSpecFile: preflightSpec}); err != nil {
			if !errors.Is(err, preflight.ErrUpgradeBlocked) {
				// The logger is not set up on this path; stderr is the pod
				// log the hook keeps on failure.
				fmt.Fprintln(os.Stderr, "upgrade preflight could not run:", err)
			}
			os.Exit(1)
		}
		return
	}

	opts := []reconciler.ReconcilerOption{
		reconciler.WithServer(reconciler.ServerConfig{Port: serverPort}),
	}

	if metricsBindAddress != "0" {
		opts = append(opts, reconciler.WithMetrics(metricsBindAddress))
	}

	if admission {
		opts = append(opts, reconciler.WithAdmission())
	}

	r, err := reconciler.New(ctx, p, opts...)
	if err != nil {
		l.Error(err, "unable to create reconciler")
		os.Exit(1)
	}

	mgr := r.GetManager()
	if err := mgr.Add(provider.NewSnapshotMirror(mgr.GetClient())); err != nil {
		l.Error(err, "unable to add the snapshot mirror")
		os.Exit(1)
	}

	if err := r.Start(ctx); err != nil {
		l.Error(err, "unable to start reconciler")
		os.Exit(1)
	}
}
