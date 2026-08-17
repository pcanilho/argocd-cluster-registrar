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
	demotedTTL        time.Duration
	execCredentials   bool

	leaderElect            bool
	leaderElectionID       string
	healthProbeBindAddress string
	metricsBindAddress     string
)

// resolveProviders turns the flags into the provider list, honouring the
// pre-0.3.0 single-provider flags. The gate is `Changed`, not emptiness:
// --secret-name-pattern and --secret-key carry non-empty defaults, so testing
// them for "" would take the legacy branch every time and ignore --provider.
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

// options is everything the flags decide, separated from using them so it is
// testable without a cluster. Warnings are returned rather than logged for the
// same reason.
type options struct {
	cfg      registrar.Config
	ctrl     registrar.ControllerOptions
	warnings []string
}

// buildOptions maps the parsed flags onto the registrar's configuration. It
// talks to nothing; everything below it in RunE needs a cluster.
func buildOptions(cmd *cobra.Command) (options, error) {
	if interval <= 0 {
		// RequeueAfter: 0 means "do not requeue", so `interval: 0s` would leave
		// every registration reconciled once and never refreshed again.
		return options{}, fmt.Errorf("--interval must be positive, got %s", interval)
	}

	providers, err := resolveProviders(cmd)
	if err != nil {
		return options{}, err
	}

	cfg := registrar.Config{
		TargetNamespace: targetNamespace,
		ManagedByValue:  managedByValue,
		Providers:       providers,
		LabelPrefix:     labelPrefix,
		DryRun:          dryRun,
		DemotedTTL:      demotedTTL,
		ExecCredentials: execCredentials,
	}
	if err := cfg.Validate(); err != nil {
		return options{}, err
	}

	var warnings []string

	// Derived unless set, so a manifest-deployed instance takes the same lease as
	// a chart-deployed one with identical configuration. Gated on Changed, not
	// emptiness: an explicit empty value is a mistake, not a request to derive.
	lease := leaderElectionID
	if !cmd.Flags().Changed("leader-election-id") {
		lease = registrar.LeaderElectionID(labelPrefix, managedByValue)
	} else if lease == "" {
		return options{}, fmt.Errorf("--leader-election-id must not be empty; omit it to derive one")
	}

	// A local, not the package variable: assigning to `leaderElect` would leak
	// into every test case that runs afterwards.
	elect := leaderElect
	if elect && dryRun {
		// A --dry-run process holding the lease reports what it would do while
		// the real one waits.
		warnings = append(warnings, "--dry-run disables leader election")
		elect = false
	}

	// Both halves of the exec gate must be on, and a half-on configuration is a
	// silent no-op in one direction and a silent refusal in the other.
	anyAllowsExec := false
	for _, p := range providers {
		if p.AllowExec {
			anyAllowsExec = true
			break
		}
	}
	switch {
	case execCredentials && !anyAllowsExec:
		warnings = append(warnings, "--exec-credentials is set but no configured provider "+
			"allows it, so nothing will be translated; the capa-eks and capz-aks presets do")
	case !execCredentials && anyAllowsExec:
		warnings = append(warnings, "a configured provider allows exec credentials but "+
			"--exec-credentials is off, so its kubeconfigs will still be refused")
	}

	if demotedTTL > 0 && demotedTTL < interval {
		// Demoted registrations are only revisited when their namespace is
		// requeued, so nothing can expire faster than the interval.
		warnings = append(warnings, fmt.Sprintf(
			"--demoted-ttl %s is shorter than --interval %s, so expiry cannot happen sooner than %s",
			demotedTTL, interval, interval))
	}

	return options{
		cfg: cfg,
		ctrl: registrar.ControllerOptions{
			Interval:               interval,
			LeaderElection:         elect,
			LeaderElectionID:       lease,
			HealthProbeBindAddress: healthProbeBindAddress,
			MetricsBindAddress:     metricsBindAddress,
		},
		warnings: warnings,
	}, nil
}

var rootCmd = &cobra.Command{
	Use: "argocd-cluster-registrar",
	// A runtime failure is not a usage error. Without these, any error out of
	// RunE prints the whole Long help after it, which in pod logs buries the
	// thing that actually went wrong.
	SilenceUsage:  true,
	SilenceErrors: false,
	Short:         "Kubernetes controller that registers child clusters with ArgoCD",
	Version:       version + " (" + commit + ") " + date,
	Long: `A controller that reconciles child-cluster kubeconfig Secrets into ArgoCD cluster Secrets.

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

Prefixed labels AND annotations on the source namespace are copied onto the
cluster Secret, so an ApplicationSet can select or template on them.

A registration is never taken over. If the Secret already exists and records a
different source namespace, or is not ours at all, it is refused and logged. An
unclaimed name contested by several namespaces goes to the oldest, and an address
another namespace already registered is refused outright, because ArgoCD
identifies a cluster by its server URL.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}
		log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level:     level,
			AddSource: debug,
		}))

		opts, err := buildOptions(cmd)
		if err != nil {
			return err
		}
		// Emitted before the --once return below, deliberately: these describe the
		// configuration, not the manager, so a pre-flight run must report them too.
		for _, w := range opts.warnings {
			log.Warn(w)
		}

		r, err := registrar.New(log, opts.cfg)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		// Report the providers actually in force, not the deprecated flag's
		// default. This line is the only startup evidence of what the process is
		// looking for, and logging `secretNamePattern` meant it always announced
		// `k3k-*-kubeconfig` no matter what --provider was set to.
		names := make([]string, 0, len(opts.cfg.Providers))
		for _, p := range opts.cfg.Providers {
			names = append(names, p.Name)
		}
		log.Info("resolved providers", slog.String("providers", strings.Join(names, ",")))

		// --once never builds a manager. It is documented as a pre-flight check to
		// run against a live cluster, so acquiring the lease would take the running
		// instance offline for the duration of a dry run.
		if once {
			return r.Reconcile(ctx)
		}

		return r.Start(ctx, opts.ctrl)
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
	f.BoolVar(&once, "once", false,
		"run a single sweep and exit, without building a manager or taking a lease")
	f.DurationVar(&interval, "interval", 60*time.Second,
		"how long before a settled cluster is revisited; bounds credential freshness, "+
			"not registration latency")
	f.StringVarP(&targetNamespace, "target-namespace", "t", "argocd",
		"namespace ArgoCD reads cluster Secrets from")
	f.StringVar(&managedByValue, "managed-by", "cluster-registrar",
		"value of the <label-prefix>managed-by label identifying namespaces to watch and Secrets to own")
	// StringArray, not StringSlice: a custom spec's key list is comma-separated,
	// and StringSlice would split it into separate providers.
	f.StringArrayVar(&providerSpecs, "provider", nil,
		"provisioner to look for; repeatable. One of "+strings.Join(registrar.PresetNames(), ", ")+
			", or a custom \"name=pattern=key[,key...][=exec]\". Defaults to k3k")
	// Superseded by --provider, kept so a 0.2.x values file keeps working. Note
	// these defaults are never empty, which is why resolveProviders gates on
	// Changed() rather than on the values.
	f.StringVar(&secretNamePattern, "secret-name-pattern", "k3k-*-kubeconfig",
		"DEPRECATED, use --provider: glob matching the kubeconfig Secret within a watched namespace")
	f.StringVar(&secretKey, "secret-key", "kubeconfig.yaml",
		"DEPRECATED, use --provider: key inside that Secret holding the kubeconfig")
	f.StringVar(&labelPrefix, "label-prefix", registrar.DefaultLabelPrefix,
		"prefix for the labels and annotations read from the source namespace and "+
			"copied onto the cluster Secret")
	f.BoolVar(&leaderElect, "leader-elect", false,
		"only reconcile while holding the lease, so two identically configured instances do not both run")
	// Derived from the configuration that decides whether two instances actually
	// collide, not from the release name: ownership is established purely by
	// label-prefix and managed-by, so instances differing only in release name
	// contend for the same Secrets and must contend for the same lease.
	f.StringVar(&leaderElectionID, "leader-election-id", "",
		"name of the Lease used for leader election, within --target-namespace; "+
			"unset derives it from --label-prefix and --managed-by")
	f.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081",
		"address serving /healthz and /readyz; empty disables it")
	// "0", not "": controller-runtime reads an empty metrics address as ":8080"
	// rather than as "off", so defaulting to empty here would open an
	// unauthenticated port on every install that never asked for one.
	f.StringVar(&metricsBindAddress, "metrics-bind-address", "0",
		`address serving Prometheus metrics; "0" disables it, which is the default`)
	// Separate from the per-provider AllowExec: that says a Secret shape is
	// exec-bearing, this says the ArgoCD deployment holds the cloud identity such
	// a registration would authenticate as.
	f.BoolVar(&execCredentials, "exec-credentials", false,
		"translate exec credential plugins (EKS, AKS) into ArgoCD's argocd-k8s-auth "+
			"configuration; only affects providers that allow it, such as capa-eks")
	// A clock on demotion is also a clock on how long a mistaken rename can be
	// undone, hence off by default.
	f.DurationVar(&demotedTTL, "demoted-ttl", 0,
		"delete a demoted registration once it has been superseded this long; 0 keeps it indefinitely")
}
