package registrar

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	coreV1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
)

const testInterval = 90 * time.Second

// secretValue reads a key from either half of a Secret.
//
// A real apiserver merges StringData into Data on write and clears StringData;
// client-go's fake does neither, so a Secret this suite CREATED has its values
// only in StringData, while one it UPDATED has them in Data (apply's
// read-modify-write writes Data directly). Tests that do not account for that
// read empty strings from freshly created Secrets and conclude something is
// broken. It also means changed() fires exactly once against the fake and never
// against a real cluster, so "steady state writes nothing" is untestable here.
func secretValue(s *coreV1.Secret, key string) string {
	if v, ok := s.Data[key]; ok {
		return string(v)
	}
	return s.StringData[key]
}

func newTestReconciler(objs ...runtime.Object) (*Reconciler, *Registrar) {
	r, _ := newTestRegistrar(objs...)
	return &Reconciler{registrar: r, interval: testInterval}, r
}

// Under a Namespace-only watch, RequeueAfter is the ONLY thing that re-reads a
// kubeconfig. A single path returning a bare ctrl.Result{} would leave that
// cluster's credentials to age out silently and surface days later as an
// authentication error with nothing in the logs.
//
// This is close to a tautology given that Reconcile has two return statements and
// both go through done(). That is the point: it can only fail if someone
// reintroduces a third, and then it fails loudly and immediately.
func TestEveryReturnPathRequeues(t *testing.T) {
	terminating := func() *coreV1.Namespace {
		ns := managedNS("k3k-x", "a")
		now := metaV1.Now()
		ns.DeletionTimestamp = &now
		ns.Finalizers = []string{"kubernetes.io/test"}
		return ns
	}
	unlabelled := &coreV1.Namespace{ObjectMeta: metaV1.ObjectMeta{
		Name:   "k3k-nolabel",
		Labels: map[string]string{ManagedByLabel(testPrefix): testManagedBy},
	}}
	unowned := &coreV1.Secret{ObjectMeta: metaV1.ObjectMeta{
		Name: "cluster-hand", Namespace: testTargetNS,
		Labels: map[string]string{argoSecretTypeLabel: argoSecretTypeValue},
	}}

	for name, tc := range map[string]struct {
		key      string
		objs     []runtime.Object
		finished bool // the key is done and must NOT be requeued
	}{
		"namespace gone, registration collected": {
			key:      "k3k-gone",
			objs:     []runtime.Object{registeredSecret("gone", "k3k-gone")},
			finished: true,
		},
		"namespace gone, nothing was ever registered": {
			key:      "k3k-never",
			finished: true,
		},
		"namespace terminating": {
			key:  "k3k-x",
			objs: []runtime.Object{terminating(), kubeconfigSecret("k3k-x", "k3k-x-kubeconfig")},
		},
		"namespace lost its ownership label": {
			key: testNS,
			objs: []runtime.Object{&coreV1.Namespace{ObjectMeta: metaV1.ObjectMeta{
				Name:   testNS,
				Labels: map[string]string{ClusterLabel(testPrefix): "a"},
			}}},
		},
		"no cluster label": {
			key:  "k3k-nolabel",
			objs: []runtime.Object{unlabelled},
		},
		"cluster name is not a valid Secret name": {
			key:  "k3k-bad",
			objs: []runtime.Object{managedNS("k3k-bad", "Prod_Cluster")},
		},
		"no kubeconfig secret yet": {
			key:  "k3k-waiting",
			objs: []runtime.Object{managedNS("k3k-waiting", "waiting")},
		},
		"kubeconfig does not parse": {
			key: "k3k-broken",
			objs: []runtime.Object{managedNS("k3k-broken", "broken"),
				secretWith("k3k-broken", "k3k-broken-kubeconfig", "kubeconfig.yaml", "<html>")},
		},
		"fresh registration": {
			key:  "k3k-new",
			objs: []runtime.Object{managedNS("k3k-new", "new"), kubeconfigSecret("k3k-new", "k3k-new-kubeconfig")},
		},
		"already up to date": {
			key: "k3k-same",
			objs: []runtime.Object{managedNS("k3k-same", "same"),
				kubeconfigSecret("k3k-same", "k3k-same-kubeconfig"), registeredSecret("same", "k3k-same")},
		},
		"refused, name is not ours": {
			key: "tenant-evil",
			objs: []runtime.Object{managedNS("tenant-evil", "hand"),
				kubeconfigSecret("tenant-evil", "k3k-evil-kubeconfig"), unowned},
		},
		"rename demotes the old registration": {
			key: "k3k-ren",
			objs: []runtime.Object{managedNS("k3k-ren", "a2"),
				kubeconfigSecret("k3k-ren", "k3k-ren-kubeconfig"), registeredSecret("a", "k3k-ren")},
		},
		"audit key": {
			key:  auditKey,
			objs: []runtime.Object{registeredSecret("a", testNS)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec, _ := newTestReconciler(tc.objs...)
			res, err := rec.Reconcile(context.Background(),
				ctrl.Request{NamespacedName: types.NamespacedName{Name: tc.key}})

			// Asserting "no error" is the point. Skipping the case on error --
			// as this test first did -- means a regression that turns a happy
			// path into a permanent error scores as a pass, and a permanently
			// erroring key is exactly how a registration ages out unnoticed.
			if err != nil {
				t.Fatalf("unexpected error, which would rate-limit this key: %v", err)
			}
			switch {
			case tc.finished && res.RequeueAfter != 0:
				t.Errorf("nothing left to do, but the key was requeued: %+v", res)
			case !tc.finished && res.RequeueAfter <= 0:
				t.Errorf("returned without scheduling a revisit: %+v", res)
			}
		})
	}
}

// Dry-run must requeue too, or a pre-flight instance reports once and then goes
// quiet while looking healthy.
func TestDryRunStillRequeues(t *testing.T) {
	rec, r := newTestReconciler(managedNS(testNS, "a"), kubeconfigSecret(testNS, "k3k-a-kubeconfig"))
	r.cfg.DryRun = true

	res, err := rec.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: testNS}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != testInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, testInterval)
	}
}

// A failure requeues rate-limited and controller-runtime DISCARDS RequeueAfter
// alongside an error, so returning both would only log a warning on every
// failure.
func TestErrorsDoNotCarryARequeue(t *testing.T) {
	rec := &Reconciler{
		registrar: &Registrar{cfg: testConfig(), log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		interval:  testInterval,
	}
	res, err := rec.done(false, io.ErrUnexpectedEOF)
	if err == nil {
		t.Fatal("expected the error through")
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v alongside an error, want 0", res.RequeueAfter)
	}
}

// The stated reason this runs continuously rather than as a Job: a k3s server
// restart rotates the child's client certificate, and a registration that is not
// re-read breaks every Application targeting that cluster with an authentication
// error rather than a visible one. Nothing tested it until now.
//
// This is what a missing RequeueAfter would eventually cost. It does not catch
// the omission itself -- it catches a refresh that is broken GIVEN a revisit --
// so it belongs next to TestEveryReturnPathRequeues, not instead of it.
func TestReconcileRefreshesRotatedKubeconfig(t *testing.T) {
	rec, r := newTestReconciler(
		managedNS(testNS, "a"), kubeconfigSecret(testNS, "k3k-a-kubeconfig"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testNS}}

	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	before, err := r.client.CoreV1().Secrets(testTargetNS).
		Get(context.Background(), "cluster-a", metaV1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := secretValue(before, "server"); got != k3kServer {
		t.Fatalf("precondition: server = %q", got)
	}

	// What a k3s server restart actually does: same endpoint, brand new client
	// certificate. The server URL staying put is the point -- if the test changed
	// it too, a registrar that only ever re-read the endpoint would still pass.
	rotated := secretWith(testNS, "k3k-a-kubeconfig", "kubeconfig.yaml",
		strings.NewReplacer(
			"client-certificate-data: Y2VydGRhdGE=", "client-certificate-data: cm90YXRlZGNlcnQ=",
			"client-key-data: a2V5ZGF0YQ==", "client-key-data: cm90YXRlZGtleQ==",
		).Replace(k3kKubeconfig))
	if _, err := r.client.CoreV1().Secrets(testNS).
		Update(context.Background(), rotated, metaV1.UpdateOptions{}); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if _, err := rec.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	after, err := r.client.CoreV1().Secrets(testTargetNS).
		Get(context.Background(), "cluster-a", metaV1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := secretValue(after, "server"); got != k3kServer {
		t.Errorf("the endpoint moved, which a rotation does not do: %q", got)
	}
	if secretValue(after, "config") == secretValue(before, "config") {
		t.Error("credentials were not refreshed after rotation; every Application " +
			"targeting this cluster would start failing authentication once the old " +
			"certificate expired")
	}
}

// A cluster name is bounded only by what a LABEL VALUE allows, 63 bytes, so
// "cluster-" + name reaches 71 -- longer than the label value demotion wants to
// write it into. Failing that write would leave the superseded registration
// still carrying ArgoCD's label, which is precisely the two-clusters-one-server
// state demotion exists to prevent, and it would fail again on every retry.
func TestDemotionSurvivesAClusterNameTooLongForALabelValue(t *testing.T) {
	long := strings.Repeat("c", 56)
	if errs := validation.IsValidLabelValue(long); len(errs) > 0 {
		t.Fatalf("precondition: %q should be a legal cluster label value: %v", long, errs)
	}
	if errs := validation.IsValidLabelValue(secretName(long)); len(errs) == 0 {
		t.Fatal("precondition: cluster-<56 chars> should be too long for a label value")
	}

	rec, r := newTestReconciler(
		managedNS("k3k-x", long), kubeconfigSecret("k3k-x", "k3k-x-kubeconfig"),
		registeredSecret("a", "k3k-x"),
	)
	if _, err := rec.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "k3k-x"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	old, err := r.client.CoreV1().Secrets(testTargetNS).
		Get(context.Background(), "cluster-a", metaV1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := old.Labels[argoSecretTypeLabel]; ok {
		t.Error("the superseded registration is still visible to ArgoCD")
	}
	if old.Labels[OrphanedSecretTypeLabel(testPrefix)] != argoSecretTypeValue {
		t.Error("secret-type was not parked")
	}
	// Every label written must be legal, or a real apiserver rejects the update
	// and demotion never happens at all. The fake does not validate.
	for k, v := range old.Labels {
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			t.Errorf("label %s=%q is not a valid label value: %v", k, v, errs)
		}
	}
}

// These are the options whose defaults are actively dangerous, asserted here
// rather than through a running manager because the failure mode is silent: the
// wrong value works fine and merely exposes something it should not.
func TestManagerOptionsPinTheDangerousDefaults(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	opts := managerOptions(testConfig(), ControllerOptions{
		Interval:               time.Minute,
		HealthProbeBindAddress: ":8081",
	}, scheme)

	if !opts.Cache.ReaderFailOnMissingInformer {
		t.Error("a cached read of an unconfigured resource would silently start a " +
			"cluster-wide informer instead of failing")
	}
	// An UNSET metrics address, which is what a zero-valued ControllerOptions and
	// any caller that forgets the field both produce. controller-runtime reads
	// empty as ":8080", so without the normalisation this is how an installation
	// that never asked for metrics ends up serving them unauthenticated.
	if opts.Metrics.BindAddress != metricsDisabled {
		t.Errorf("metrics BindAddress = %q for an unset option; anything but %q "+
			"serves :8080 unauthenticated", opts.Metrics.BindAddress, metricsDisabled)
	}
	if opts.Client.Cache == nil || len(opts.Client.Cache.DisableFor) == 0 {
		t.Fatal("Secrets are not excluded from the cached client")
	}
	if _, ok := opts.Client.Cache.DisableFor[0].(*coreV1.Secret); !ok {
		t.Errorf("DisableFor[0] = %T, want *v1.Secret", opts.Client.Cache.DisableFor[0])
	}
	// Only namespaces may be cached; a Secret entry here would mean an informer
	// holding every child cluster's credentials in memory. The map is keyed by
	// object pointer, so it has to be walked by type rather than looked up.
	if len(opts.Cache.ByObject) != 1 {
		t.Errorf("cache configured for %d kinds, want exactly 1", len(opts.Cache.ByObject))
	}
	for k, byObj := range opts.Cache.ByObject {
		if _, ok := k.(*coreV1.Namespace); !ok {
			t.Errorf("cache configured for %T; only Namespace may be", k)
			continue
		}
		if byObj.Label == nil || byObj.Label.Empty() {
			t.Error("the Namespace cache is unfiltered; it would hold every namespace " +
				"in the cluster")
		}
	}
	if opts.LeaderElectionNamespace != testTargetNS {
		t.Errorf("LeaderElectionNamespace = %q, want %q; unset means the pod's own "+
			"namespace, which is not where two colliding releases meet",
			opts.LeaderElectionNamespace, testTargetNS)
	}
}

// Metrics must still be servable when asked for, or the normalisation above
// could be "fixed" by hardcoding the disabled value and nothing would notice.
//
// The port is one that appears nowhere else in this package: passing through
// HealthProbeBindAddress by mistake would otherwise satisfy an assertion that
// only checked the value was not "0".
func TestMetricsBindAddressIsPassedThroughWhenSet(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	opts := managerOptions(testConfig(), ControllerOptions{
		Interval:               time.Minute,
		HealthProbeBindAddress: ":8081",
		MetricsBindAddress:     ":9090",
	}, scheme)

	if opts.Metrics.BindAddress != ":9090" {
		t.Errorf("metrics BindAddress = %q, want :9090; the option does not reach the manager",
			opts.Metrics.BindAddress)
	}
}
