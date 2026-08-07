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

// auditKey is the reconcile key reserved for the unrouted-Secret audit.
//
// A Namespace request always carries a name, so the empty name cannot collide
// with a real one. Giving the audit a key rather than running it once at startup
// means it is retried on the same schedule as everything else, which matters
// because the Secrets it reports are created by hand at runtime, not inherited
// from an old version.
const auditKey = ""

// seedBuffer bounds the startup seeding channel. Overflow is not a correctness
// problem -- the seeder blocks until the controller drains it -- so this only
// needs to be large enough that a normal fleet does not serialise on it.
const seedBuffer = 256

// metricsDisabled is the value controller-runtime treats as "do not serve".
const metricsDisabled = "0"

// ControllerOptions configures the manager. Everything here is operational; what
// the registrar actually does is in Config.
type ControllerOptions struct {
	// Interval is how often a key is revisited once it has settled. Under a
	// watch this is the ONLY thing that re-reads a kubeconfig, so it is what
	// bounds how stale a registration can get after a certificate rotation.
	Interval time.Duration

	// LeaderElection guards against two instances configured identically. It is
	// off by default: enabling it needs RBAC for leases and events in
	// TargetNamespace, which is often a namespace the operator does not own.
	LeaderElection   bool
	LeaderElectionID string

	// HealthProbeBindAddress serves /healthz and /readyz. Empty disables it.
	HealthProbeBindAddress string

	// RestConfig overrides the ambient cluster connection. Nil, the normal case,
	// means in-cluster config or the caller's own kubeconfig. Set by tests that
	// drive a real manager against envtest.
	RestConfig *rest.Config

	// MetricsBindAddress serves Prometheus metrics. "0" disables it, and that is
	// the default.
	//
	// Note the asymmetry with HealthProbeBindAddress above, which is not an
	// oversight and must not be tidied away: controller-runtime reads an EMPTY
	// metrics address as ":8080" rather than as "off", so the two fields disable
	// on different values. managerOptions normalises empty to "0" so that a
	// zero-valued ControllerOptions cannot open an unauthenticated port.
	MetricsBindAddress string
}

// Reconciler adapts a Registrar to controller-runtime.
//
// Note what it does NOT do: reconcileOne and AuditUnrouted return only an error.
// Deciding when to come back is this type's job and happens in exactly one place,
// so no code path can forget to schedule the next visit. Under a Namespace-only
// watch that omission is not cosmetic -- RequeueAfter is the only thing that
// re-reads a kubeconfig, so a single bare return would let one cluster's
// credentials age out silently and surface days later as an auth error.
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

// done is the only place a Result is constructed.
//
// On error the Result is deliberately empty: controller-runtime discards
// RequeueAfter when an error is returned and requeues rate-limited instead, so
// setting both would just log a warning on every failure.
//
// `finished` drops the key. Only a namespace that is provably gone and owns
// nothing further qualifies, because everything else has a reason to come back:
// a live cluster needs its credentials refreshed, and a namespace that merely
// stopped being ours has to stay queued, since the filtered cache has already
// forgotten it and will never report its deletion. Without this every namespace
// ever seen would be revisited forever, each visit costing a LIST of the ArgoCD
// namespace -- which the sweep never did, because it recomputed its key set from
// scratch each pass.
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

// scheme carries only what is watched. corev1 is the whole of it: this tool has
// no CRDs and reads nothing else.
func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("build scheme: %w", err)
	}
	return s, nil
}

// managerOptions is separate so the options with dangerous defaults can be
// asserted without standing up a manager at all.
func managerOptions(cfg Config, opts ControllerOptions, scheme *runtime.Scheme) ctrl.Options {
	// Empty means "off" here, even though controller-runtime reads it as ":8080".
	// Without this, a ControllerOptions that simply does not mention metrics --
	// a zero value, a caller that forgot the field, a test -- would serve an
	// unauthenticated endpoint on a port nobody chose. The flag default is "0" as
	// well; both are wanted, because either alone can be undone by a plausible
	// edit to the other.
	metricsBind := opts.MetricsBindAddress
	if metricsBind == "" {
		metricsBind = metricsDisabled
	}

	return ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			// A cached read of anything we forgot to configure is an error, not a
			// new cluster-wide informer started silently in the background.
			ReaderFailOnMissingInformer: true,
			ByObject: map[client.Object]cache.ByObject{
				&coreV1.Namespace{}: {
					Label: labels.SelectorFromSet(labels.Set{
						ManagedByLabel(cfg.LabelPrefix): cfg.ManagedByValue,
					}),
				},
			},
			// Nothing reads managedFields and they dominate the size of a cached
			// object.
			DefaultTransform: cache.TransformStripManagedFields(),
		},
		// Belt and braces: no Secret may ever be served from cache. Every read
		// here goes through the clientset, so this should be unreachable.
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: []client.Object{&coreV1.Secret{}}},
		},
		// Served unauthenticated when enabled, deliberately. Protecting it means
		// controller-runtime's authn/authz filter, which drags in k8s.io/apiserver
		// and needs tokenreviews/subjectaccessreviews RBAC; the four series here
		// are counts of the instance's own decisions and carry no cluster
		// identity, so a NetworkPolicy is the proportionate control.
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
// ARCHITECTURE, because this is the part a contributor will try to "fix":
//
// Only Namespaces are watched. Source kubeconfig Secrets deliberately are NOT,
// even though a metadata watch would see a Data-only change. k3k regenerates the
// child's keypair on every one of its own reconciles, so that Secret changes far
// more often than the credential meaningfully does; the interval below is acting
// as a rate limiter, and watching the source would turn every k3k reconcile into
// a write against a credential-bearing Secret in the ArgoCD namespace. Such a
// watch could not be narrowed either -- the provisioner owns that Secret, so it
// carries none of our labels, which is the same reason discovery is driven by
// the namespace in the first place.
//
// The manager's cached client is never used for reads. Everything goes through
// the clientset, because the namespace existence proof must not come from a
// label-filtered cache: the apiserver reports an object that stops matching a
// selector as a synthetic Delete, so a cached NotFound cannot tell a deleted
// namespace from one that merely lost a label. ReaderFailOnMissingInformer turns
// any accidental cached read into an error rather than a silent new informer.
func (r *Registrar) Start(ctx context.Context, opts ControllerOptions) error {
	if opts.Interval <= 0 {
		return fmt.Errorf("interval must be positive, got %s", opts.Interval)
	}

	scheme, err := newScheme()
	if err != nil {
		return err
	}
	// Injected config wins, mirroring the client seam below. Without it Start can
	// only ever be run against the ambient cluster, which is what kept the manager
	// wiring out of reach of the test suite.
	base := opts.RestConfig
	if base == nil {
		var err error
		if base, err = BaseRestConfig(); err != nil {
			return err
		}
	}

	// controller-runtime logs through logr. Without this it prints one warning to
	// stderr after thirty seconds and then discards everything it would have said,
	// including the queue and leader-election detail that makes a failure legible.
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

	// Only build a client if the caller did not supply one. Overwriting an
	// injected client would make Start untestable -- a test passing a fake would
	// silently talk to the ambient cluster instead.
	if r.client == nil {
		kube, err := ClientFor(base)
		if err != nil {
			return err
		}
		r.client = kube
	}

	rec := &Reconciler{registrar: r, interval: opts.Interval}

	// Seeded keys arrive through a channel source rather than the cache, because
	// the whole point is to reconcile keys the cache cannot know about.
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

	// The watch alone cannot see what is already gone. A namespace deleted while
	// this was not running never appears in the initial list, so nothing would
	// ever revisit it and its registration would strand forever. SyncPeriod is no
	// help: upstream is explicit that it replays what is already cached rather
	// than reconciling the cache against the server.
	//
	// So seed the queue once from the same key set the sweep uses, which includes
	// namespaces known only from the registrations they left behind.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		defer close(seeds)
		// Retry rather than return. An error here reaches the manager's error
		// channel and tears the process down, so one API blip at the wrong second
		// would take out a healthy leader and, with ReleaseOnCancel, drop the
		// lease and force a re-election.
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
		// The audit key last: it is the cheapest and the least urgent.
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
