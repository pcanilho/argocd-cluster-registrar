// Package cmd contains the command-line interface for the application.
package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pcanilho/argocd-cluster-registrar/internal/registrar"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
	providerSpecs     []string
	secretNamePattern string
	secretKey         string
	labelPrefix       string

	leaderElect            bool
	leaderElectionID       string
	healthProbeBindAddress string
)

// resolveProviders turns the flags into the provider list, honouring the
// pre-0.3.0 single-provider flags.
//
// The gate is `Changed`, NOT emptiness, and that distinction is the whole point:
// --secret-name-pattern and --secret-key both carry non-empty defaults, so they
// are never unset. Testing them for "" would take the legacy branch every single
// time and silently ignore --provider entirely -- the same class of quiet
// misconfiguration the 0.1.x migration note warns about, which is precisely what
// this compatibility path exists to avoid.
func resolveProviders(cmd *cobra.Command) ([]registrar.Provider, error) {
	legacy := cmd.Flags().Changed("secret-name-pattern") || cmd.Flags().Changed("secret-key")

	if len(providerSpecs) > 0 {
		if legacy {
			return nil, fmt.Errorf(
				"--provider cannot be combined with --secret-name-pattern/--secret-key; " +
					"express the old flags as a provider spec instead")
		}
		out := make([]registrar.Provider, 0, len(providerSpecs))
		for _, spec := range providerSpecs {
			p, err := registrar.ParseProvider(spec)
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, nil
	}

	if legacy {
		return []registrar.Provider{{
			Name:              "custom",
			SecretNamePattern: secretNamePattern,
			SecretKeys:        []string{secretKey},
		}}, nil
	}

	// Neither set: the shipped default, unchanged from 0.2.x.
	p, _ := registrar.Preset("k3k")
	return []registrar.Provider{p}, nil
}

var rootCmd = &cobra.Command{
	Use:     "cluster-registrar",
	Short:   "Register child Kubernetes clusters with ArgoCD",
	Version: version + " (" + commit + ") " + date,
	Long: `Reconciles child-cluster kubeconfig Secrets into ArgoCD cluster Secrets.

Discovery is namespace-driven: any namespace labelled
` + registrar.ManagedByLabel(registrar.DefaultLabelPrefix) + `=<value> is inspected for a Secret
matching one of the configured --provider shapes, whose kubeconfig is reshaped
into an ArgoCD cluster Secret in --target-namespace.

That indirection exists because the provisioner -- not this tool -- creates the
kubeconfig Secret, so it carries none of our labels. The namespace is the
nearest object we control, so per-cluster intent lives there.

The tool is provisioner-neutral. Anything that writes a kubeconfig into a Secret
works; presets ship for ` + strings.Join(registrar.PresetNames(), ", ") + `, and --provider takes several at
once so one instance can serve a mixed fleet.

Secrets whose source namespace has gone away are DELETED, so a destroyed cluster
does not leave a broken entry behind in ArgoCD. Renaming a cluster instead
DEMOTES the old Secret: it is hidden from ArgoCD but kept, so reverting the
rename restores it.

A registration is never taken over. If the Secret already exists and records a
different source namespace, or is not ours at all, it is refused and logged. An
unclaimed name contested by several namespaces goes to the oldest.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}
		log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level:     level,
			AddSource: debug,
		}))

		if interval <= 0 {
			// RequeueAfter: 0 means "do not requeue", so a Helm value of
			// `interval: 0s` would leave every registration reconciled once and
			// then never refreshed again -- silently, until a certificate
			// rotation broke it days later.
			return fmt.Errorf("--interval must be positive, got %s", interval)
		}

		providers, err := resolveProviders(cmd)
		if err != nil {
			return err
		}

		cfg := registrar.Config{
			TargetNamespace: targetNamespace,
			ManagedByValue:  managedByValue,
			Providers:       providers,
			LabelPrefix:     labelPrefix,
			DryRun:          dryRun,
		}
		if err := cfg.Validate(); err != nil {
			return err
		}

		r, err := registrar.New(log, cfg)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		// Report the providers actually in force, not the deprecated flag's
		// default. This line is the only startup evidence of what the process is
		// looking for, and logging `secretNamePattern` meant it always announced
		// `k3k-*-kubeconfig` no matter what --provider was set to.
		names := make([]string, 0, len(providers))
		for _, p := range providers {
			names = append(names, p.Name)
		}
		log.Info("resolved providers", slog.String("providers", strings.Join(names, ",")))

		// --once never builds a manager. It is documented as a pre-flight check to
		// run against a live cluster, so acquiring the lease would take the running
		// instance offline for the duration of a dry run.
		if once {
			return r.Reconcile(ctx)
		}

		if leaderElect && dryRun {
			// Same hazard, slower: a --dry-run process holding the lease is a
			// registrar that reports what it would do while the real one waits.
			log.Warn("--dry-run disables leader election")
			leaderElect = false
		}

		return r.Start(ctx, registrar.ControllerOptions{
			Interval:               interval,
			LeaderElection:         leaderElect,
			LeaderElectionID:       leaderElectionID,
			HealthProbeBindAddress: healthProbeBindAddress,
		})
	},
}

func init() {
	registerFlags(rootCmd.PersistentFlags())
}

// registerFlags is separate from init so that tests can build a command with the
// real flag definitions rather than a copy that drifts from them. The `Changed`
// gate in resolveProviders is only meaningful against flags declared exactly as
// they are here, defaults included.
func registerFlags(f *pflag.FlagSet) {
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
	// StringArray, not StringSlice: a custom spec's key list is comma-separated,
	// and StringSlice would split it into separate providers.
	f.StringArrayVar(&providerSpecs, "provider", nil,
		"provisioner to look for; repeatable. One of "+strings.Join(registrar.PresetNames(), ", ")+
			", or a custom \"name=pattern=key[,key...]\". Defaults to k3k")
	// Superseded by --provider, kept so a 0.2.x values file keeps working. Note
	// these defaults are never empty, which is why resolveProviders gates on
	// Changed() rather than on the values.
	f.StringVar(&secretNamePattern, "secret-name-pattern", "k3k-*-kubeconfig",
		"DEPRECATED, use --provider: glob matching the kubeconfig Secret within a watched namespace")
	f.StringVar(&secretKey, "secret-key", "kubeconfig.yaml",
		"DEPRECATED, use --provider: key inside that Secret holding the kubeconfig")
	f.StringVar(&labelPrefix, "label-prefix", registrar.DefaultLabelPrefix,
		"prefix for the labels read from the source namespace and copied onto the cluster Secret")
	f.BoolVar(&leaderElect, "leader-elect", false,
		"only reconcile while holding the lease, so two identically configured instances do not both run")
	// Derived from the configuration that decides whether two instances actually
	// collide, NOT from the release name: ownership is established purely by
	// label-prefix and managed-by, so instances differing only in release name
	// contend for the same Secrets and must contend for the same lease.
	f.StringVar(&leaderElectionID, "leader-election-id", "argocd-cluster-registrar",
		"name of the Lease used for leader election, within --target-namespace")
	f.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081",
		"address serving /healthz and /readyz; empty disables it")
}
