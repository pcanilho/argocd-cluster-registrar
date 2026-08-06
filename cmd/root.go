// Package cmd contains the command-line interface for the application.
package cmd

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pcanilho/argocd-cluster-registrar/internal/registrar"
	"github.com/spf13/cobra"
)

var (
	version = "dev"
	commit  = "n/a"
	date    = "n/a"
)

// Execute runs the command.
func Execute() error {
	return rootCmd.Execute()
}

var (
	debug             bool
	once              bool
	dryRun            bool
	interval          time.Duration
	targetNamespace   string
	managedByValue    string
	secretNamePattern string
	secretKey         string
	labelPrefix       string
)

var rootCmd = &cobra.Command{
	Use:     "cluster-registrar",
	Short:   "Register child Kubernetes clusters with ArgoCD",
	Version: version + " (" + commit + ") " + date,
	Long: `Reconciles child-cluster kubeconfig Secrets into ArgoCD cluster Secrets.

Discovery is namespace-driven: any namespace labelled
` + registrar.ManagedByLabel(registrar.DefaultLabelPrefix) + `=<value> is inspected for a Secret matching
--secret-name-pattern, whose kubeconfig is reshaped into an ArgoCD cluster
Secret in --target-namespace.

That indirection exists because the provisioner -- not this tool -- creates the
kubeconfig Secret, so it carries none of our labels. The namespace is the
nearest object we control, so per-cluster intent lives there.

Nothing here is vcluster-specific: k3k, plain k3s and CAPI-style providers all
publish a kubeconfig Secret, and any of them work by adjusting the pattern and
key.

Secrets whose source namespace has gone away are DELETED, so a destroyed cluster
does not leave a broken entry behind in ArgoCD.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}
		log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level:     level,
			AddSource: debug,
		}))

		r, err := registrar.New(log, registrar.Config{
			TargetNamespace:   targetNamespace,
			ManagedByValue:    managedByValue,
			SecretNamePattern: secretNamePattern,
			SecretKey:         secretKey,
			LabelPrefix:       labelPrefix,
			DryRun:            dryRun,
		})
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		if once {
			return r.Reconcile(ctx)
		}

		log.Info("starting reconcile loop",
			slog.Duration("interval", interval),
			slog.String("targetNamespace", targetNamespace),
			slog.String("managedBy", managedByValue),
			slog.String("secretNamePattern", secretNamePattern))

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			// A failed pass is logged, never fatal: one unreachable child must not
			// take the registrar down for every other cluster.
			if err := r.Reconcile(ctx); err != nil {
				log.Error("reconcile failed", slog.Any("error", err))
			}
			select {
			case <-ctx.Done():
				log.Info("shutting down")
				return nil
			case <-ticker.C:
			}
		}
	},
}

func init() {
	f := rootCmd.PersistentFlags()
	f.BoolVar(&debug, "debug", false, "enable debug logging")
	f.BoolVar(&dryRun, "dry-run", false, "log intended changes without writing")
	// Long-running is the default. A one-shot Job cannot garbage-collect, because
	// a cluster is usually destroyed long after the last sync that created it.
	f.BoolVar(&once, "once", false, "run a single reconcile pass and exit")
	f.DurationVar(&interval, "interval", 60*time.Second, "reconcile interval")
	f.StringVarP(&targetNamespace, "target-namespace", "t", "argocd",
		"namespace ArgoCD reads cluster Secrets from")
	f.StringVar(&managedByValue, "managed-by", "cluster-registrar",
		"value of the <label-prefix>managed-by label identifying namespaces to watch and Secrets to own")
	f.StringVar(&secretNamePattern, "secret-name-pattern", "k3k-*-kubeconfig",
		"glob matching the kubeconfig Secret within a watched namespace")
	f.StringVar(&secretKey, "secret-key", "kubeconfig.yaml",
		"key inside that Secret holding the kubeconfig")
	f.StringVar(&labelPrefix, "label-prefix", registrar.DefaultLabelPrefix,
		"prefix for the labels read from the source namespace and copied onto the cluster Secret")
}
