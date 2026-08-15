package registrar

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-logr/logr"
	coreV1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// auditKey is the reconcile key reserved for the unrouted-Secret audit. A
// Namespace request always carries a name, so the empty one cannot collide. A key
// rather than a startup task, because the Secrets it reports appear at runtime.
const auditKey = ""

// seedBuffer bounds the startup seeding channel. Overflow is not a correctness
// problem, since the seeder blocks until the controller drains it.
const seedBuffer = 256

// metricsDisabled is the value controller-runtime treats as "do not serve".
const metricsDisabled = "0"

// ControllerOptions configures the manager. Everything here is operational; what
// the registrar actually does is in Config.
type ControllerOptions struct {
	// Interval is how often a key is revisited once it has settled. Under a
	// watch this is the only thing that re-reads a kubeconfig, so it is what
	// bounds how stale a registration can get after a certificate rotation.
	Interval time.Duration

	// LeaderElection guards against two instances configured identically. It is
	// off by default: enabling it needs RBAC for leases and events in
	// TargetNamespace, which is often a namespace the operator does not own.
	LeaderElection   bool
	LeaderElectionID string

	// HealthProbeBindAddress serves /healthz and /readyz. Empty disables it.
	HealthProbeBindAddress string

	// RestConfig overrides the connection the manager uses; nil means in-cluster
	// config or the caller's kubeconfig. It does not redirect the clientset, so
	// pair it with NewWithClient and a client from the same config, or the watch
	// and the reads point at different clusters.
	RestConfig *rest.Config

	// MetricsBindAddress serves Prometheus metrics; "0" disables it and is the
	// default. The asymmetry with HealthProbeBindAddress is real: controller-runtime
	// reads an empty metrics address as ":8080", so managerOptions normalises empty
	// to "0" and a zero value cannot open an unauthenticated port.
	MetricsBindAddress string
}

// Reconciler adapts a Registrar to controller-runtime. ReconcileOne and
// AuditUnrouted return only an error; when to come back is decided here, in one
// place, so no path can forget it. Under a Namespace-only watch RequeueAfter is
// the only thing that re-reads a kubeconfig.
type Reconciler struct {
	registrar *Registrar
	interval  time.Duration
}

// Reconcile has exactly two return statements and both go through done.
func (rc *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name == auditKey {
		return rc.done(false, rc.registrar.AuditUnrouted(ctx))
	}
	return rc.done(rc.registrar.ReconcileOne(ctx, req.Name))
}

// done is the only place a Result is constructed. On error it is empty, because
// controller-runtime discards RequeueAfter and requeues rate-limited anyway.
//
// `finished` drops the key, and only a namespace provably gone and owning nothing
// qualifies: a live cluster needs its credentials refreshed, and one that merely
// stopped being ours must stay queued, since the filtered cache has forgotten it
// and will never report its deletion.
func (rc *Reconciler) done(finished bool, err error) (ctrl.Result, error) {
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case finished:
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{RequeueAfter: rc.interval}, nil
	}
}

// scheme carries only what is watched: corev1, since this tool has no CRDs.
func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("build scheme: %w", err)
	}
	return s, nil
}

// managerOptions is separate so options with dangerous defaults can be asserted
// without standing up a manager.
func managerOptions(cfg Config, opts ControllerOptions, scheme *runtime.Scheme) ctrl.Options {
	// Empty means off here, even though controller-runtime reads it as ":8080",
	// so a zero-valued ControllerOptions cannot serve an unauthenticated endpoint
	// on a port nobody chose. The flag default is "0" as well; both are wanted.
	metricsBind := opts.MetricsBindAddress
	if metricsBind == "" {
		metricsBind = metricsDisabled
	}

	return ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			// A cached read of anything unconfigured is an error, not a new
			// cluster-wide informer started silently.
			ReaderFailOnMissingInformer: true,
			ByObject: map[client.Object]cache.ByObject{
				&coreV1.Namespace{}: {
					Label: labels.SelectorFromSet(labels.Set{
						ManagedByLabel(cfg.LabelPrefix): cfg.ManagedByValue,
					}),
				},
			},
			// They dominate cached object size and nothing reads them.
			DefaultTransform: cache.TransformStripManagedFields(),
		},
		// Belt and braces: no Secret may ever be served from cache. Every read
		// here goes through the clientset, so this should be unreachable.
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: []client.Object{&coreV1.Secret{}}},
		},
		// Served unauthenticated when enabled. Protecting it means
		// controller-runtime's authn/authz filter, which drags in k8s.io/apiserver;
		// these series carry no cluster identity, so a NetworkPolicy is
		// proportionate.
		Metrics:                       metricsserver.Options{BindAddress: metricsBind},
		HealthProbeBindAddress:        opts.HealthProbeBindAddress,
		LeaderElection:                opts.LeaderElection,
		LeaderElectionID:              opts.LeaderElectionID,
		LeaderElectionNamespace:       cfg.TargetNamespace,
		LeaderElectionReleaseOnCancel: true,
	}
}

// Start runs the controller until ctx is cancelled.
//
// Only Namespaces are watched, not the source kubeconfig Secrets. k3k regenerates
// the child's keypair on every one of its reconciles, so that Secret changes far
// more often than the credential does; the interval is a rate limiter, and
// watching the source would turn every k3k reconcile into a write against a
// credential-bearing Secret in the ArgoCD namespace. Nor could the watch be
// narrowed, since the provisioner owns that Secret.
//
// The manager's cached client is never used for reads, because the namespace
// existence proof must not come from a label-filtered cache: a cached NotFound
// cannot tell a deleted namespace from one that merely lost a label.
// ReaderFailOnMissingInformer turns any accidental cached read into an error.
func (r *Registrar) Start(ctx context.Context, opts ControllerOptions) error {
	if opts.Interval <= 0 {
		return fmt.Errorf("interval must be positive, got %s", opts.Interval)
	}

	scheme, err := newScheme()
	if err != nil {
		return err
	}
	// Injected config wins, mirroring the client seam below; without it Start can
	// only run against the ambient cluster and the manager wiring stays untestable.
	base := opts.RestConfig
	if base == nil {
		var err error
		if base, err = BaseRestConfig(); err != nil {
			return err
		}
	}

	// Without this, controller-runtime warns once to stderr after 30s and then
	// discards everything it would have said.
	ctrl.SetLogger(logr.FromSlogHandler(r.log.Handler()))

	mgr, err := ctrl.NewManager(base, managerOptions(r.cfg, opts, scheme))
	if err != nil {
		return fmt.Errorf("build manager: %w", err)
	}

	if opts.HealthProbeBindAddress != "" {
		if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
			return fmt.Errorf("add healthz: %w", err)
		}
		if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
			return fmt.Errorf("add readyz: %w", err)
		}
	}

	// Overwriting an injected client would make Start untestable: a fake would
	// silently talk to the ambient cluster instead.
	if r.client == nil {
		kube, err := ClientFor(base)
		if err != nil {
			return err
		}
		r.client = kube
	}

	rec := &Reconciler{registrar: r, interval: opts.Interval}

	// Through a channel rather than the cache, the point being to reconcile keys
	// the cache cannot know about.
	seeds := make(chan event.TypedGenericEvent[*coreV1.Namespace], seedBuffer)

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("cluster-registrar").
		WatchesRawSource(source.Kind(mgr.GetCache(), &coreV1.Namespace{},
			&handler.TypedEnqueueRequestForObject[*coreV1.Namespace]{})).
		WatchesRawSource(source.Channel(seeds,
			&handler.TypedEnqueueRequestForObject[*coreV1.Namespace]{})).
		Complete(rec); err != nil {
		return fmt.Errorf("build controller: %w", err)
	}

	// The watch cannot see what is already gone: a namespace deleted while this
	// was not running never appears in the initial list, so its registration would
	// strand forever. SyncPeriod replays the cache rather than reconciling it
	// against the server. So seed once from the sweep's key set, which includes
	// namespaces known only from the registrations they left behind.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		defer close(seeds)
		// Retry rather than return: an error here tears the process down, so one
		// API blip would drop a healthy leader's lease and force a re-election.
		keys := make([]string, 0, seedBuffer)
		if err := wait.PollUntilContextCancel(ctx, 2*time.Second, true,
			func(ctx context.Context) (bool, error) {
				var err error
				keys, err = r.reconcileKeys(ctx)
				if err != nil {
					r.log.Warn("could not seed reconcile queue; retrying",
						slog.Any("error", err))
					return false, nil
				}
				return true, nil
			}); err != nil {
			return nil // context cancelled: shutting down, not a failure
		}
		// Last: cheapest and least urgent.
		keys = append(keys, auditKey)
		r.log.Info("seeding reconcile queue", slog.Int("keys", len(keys)))
		for _, k := range keys {
			select {
			case seeds <- event.TypedGenericEvent[*coreV1.Namespace]{
				Object: &coreV1.Namespace{ObjectMeta: metaV1.ObjectMeta{Name: k}},
			}:
			case <-ctx.Done():
				return nil
			}
		}
		return nil
	})); err != nil {
		return fmt.Errorf("add seeder: %w", err)
	}

	r.log.Info("starting controller",
		slog.Duration("interval", opts.Interval),
		slog.String("targetNamespace", r.cfg.TargetNamespace),
		slog.String("managedBy", r.cfg.ManagedByValue),
		slog.Bool("leaderElection", opts.LeaderElection))

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	return nil
}
