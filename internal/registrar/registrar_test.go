package registrar

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	coreV1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testPrefix = "example.com/"
	// Shared by both test files: it is the ownership marker the registrar
	// discovers namespaces by and garbage collects on.
	testManagedBy = "cluster-registrar"
)

func testConfig() Config {
	k3k, _ := Preset("k3k")
	return Config{
		TargetNamespace: "argocd",
		ManagedByValue:  testManagedBy,
		Providers:       []Provider{k3k},
		LabelPrefix:     testPrefix,
	}
}

// mustPresets is a shorthand for building a provider list in tests.
func mustPresets(t *testing.T, names ...string) []Provider {
	t.Helper()
	out := make([]Provider, 0, len(names))
	for _, n := range names {
		p, ok := Preset(n)
		if !ok {
			t.Fatalf("unknown preset %q", n)
		}
		out = append(out, p)
	}
	return out
}

func newTestRegistrar(objs ...runtime.Object) (*Registrar, *fake.Clientset) {
	c := fake.NewClientset(objs...)
	return &Registrar{
		client: c,
		cfg:    testConfig(),
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, c
}

func managedNS(name, cluster string) *coreV1.Namespace {
	return &coreV1.Namespace{ObjectMeta: metaV1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			ManagedByLabel(testPrefix): testManagedBy,
			ClusterLabel(testPrefix):   cluster,
		},
	}}
}

func kubeconfigSecret(ns, name string) *coreV1.Secret {
	return &coreV1.Secret{
		ObjectMeta: metaV1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{"kubeconfig.yaml": []byte(k3kKubeconfig)},
	}
}

// registeredSecret is what a previous pass would have written.
func registeredSecret(cluster, srcNS string) *coreV1.Secret {
	return &coreV1.Secret{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      secretName(cluster),
			Namespace: "argocd",
			Labels: map[string]string{
				argoSecretTypeLabel:              argoSecretTypeValue,
				ManagedByLabel(testPrefix):       testManagedBy,
				ClusterLabel(testPrefix):         cluster,
				SourceNamespaceLabel(testPrefix): srcNS,
			},
		},
		Data: map[string][]byte{"name": []byte(cluster), "server": []byte("https://x"), "config": []byte("{}")},
	}
}

func secretExists(t *testing.T, c *fake.Clientset, name string) bool {
	t.Helper()
	_, err := c.CoreV1().Secrets("argocd").Get(context.Background(), name, metaV1.GetOptions{})
	if err != nil && !apiErrors.IsNotFound(err) {
		t.Fatalf("unexpected error: %v", err)
	}
	return err == nil
}

// The catastrophe this guard exists for: a transient API error must never be
// read as "the cluster was deleted". Before the fix, one failing List emptied
// `desired` and collect() deregistered the whole fleet, while logging the
// failure as the reassuring "no kubeconfig secret yet".
func TestTransientSecretListErrorDoesNotDeregisterAnything(t *testing.T) {
	r, c := newTestRegistrar(
		managedNS("k3k-a", "a"), kubeconfigSecret("k3k-a", "k3k-a-kubeconfig"), registeredSecret("a", "k3k-a"),
		managedNS("k3k-b", "b"), kubeconfigSecret("k3k-b", "k3k-b-kubeconfig"), registeredSecret("b", "k3k-b"),
	)
	c.PrependReactor("list", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() != "argocd" {
			return true, nil, apiErrors.NewInternalError(io.ErrUnexpectedEOF)
		}
		return false, nil, nil
	})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Logf("reconcile reported: %v", err)
	}

	for _, n := range []string{"cluster-a", "cluster-b"} {
		if !secretExists(t, c, n) {
			t.Errorf("%s was deleted after a transient API error; the fleet would have been deregistered", n)
		}
	}
}

// Even with a complete pass, deletion needs the source namespace to be provably
// gone rather than merely absent from the desired set.
func TestCollectRequiresProofTheNamespaceIsGone(t *testing.T) {
	// The namespace exists but carries no cluster label, so it yields no child.
	unlabelled := &coreV1.Namespace{ObjectMeta: metaV1.ObjectMeta{
		Name:   "k3k-a",
		Labels: map[string]string{ManagedByLabel(testPrefix): testManagedBy},
	}}
	r, c := newTestRegistrar(unlabelled, registeredSecret("a", "k3k-a"))

	if err := r.collect(context.Background(), nil, nil); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("cluster-a deleted while its source namespace still exists")
	}
}

func TestCollectDeletesWhenNamespaceIsActuallyGone(t *testing.T) {
	r, c := newTestRegistrar(registeredSecret("gone", "k3k-gone"))
	if err := r.collect(context.Background(), nil, nil); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if secretExists(t, c, "cluster-gone") {
		t.Error("cluster-gone should have been deleted once its namespace was NotFound")
	}
}

// The safety property asserted in the package comment: GC only ever touches what
// it owns.
func TestCollectNeverTouchesUnlabelledSecrets(t *testing.T) {
	hand := &coreV1.Secret{ObjectMeta: metaV1.ObjectMeta{
		Name:      "cluster-handmade",
		Namespace: "argocd",
		Labels:    map[string]string{argoSecretTypeLabel: argoSecretTypeValue},
	}}
	r, c := newTestRegistrar(hand)
	if err := r.collect(context.Background(), nil, nil); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !secretExists(t, c, "cluster-handmade") {
		t.Error("a hand-registered cluster Secret was deleted")
	}
}

// One undeletable Secret must not stall GC for every other cluster.
func TestCollectContinuesPastDeleteErrors(t *testing.T) {
	r, c := newTestRegistrar(registeredSecret("a", "k3k-a"), registeredSecret("b", "k3k-b"))
	c.PrependReactor("delete", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.DeleteAction).GetName() == "cluster-a" {
			return true, nil, apiErrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "cluster-a", io.ErrUnexpectedEOF)
		}
		return false, nil, nil
	})

	if err := r.collect(context.Background(), nil, nil); err == nil {
		t.Error("expected the forbidden delete to be reported")
	}
	if secretExists(t, c, "cluster-b") {
		t.Error("cluster-b was not deleted; one failure stalled the whole GC pass")
	}
}

// Update replaces the whole object, so a freshly-built Secret would silently drop
// ArgoCD's own fields and any operator-set annotations.
func TestApplyPreservesForeignFieldsAndAnnotations(t *testing.T) {
	existing := registeredSecret("a", "k3k-a")
	existing.Data["namespaces"] = []byte("team-a")
	existing.Data["project"] = []byte("infra")
	existing.Annotations = map[string]string{"managed-by": "argocd.argoproj.io"}
	existing.Labels["unrelated"] = "keepme"

	r, c := newTestRegistrar(existing)
	err := r.apply(context.Background(), child{
		cluster: "a", namespace: "k3k-a",
		server: "https://new", config: `{"tlsClientConfig":{"insecure":false}}`,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	got, err := c.CoreV1().Secrets("argocd").Get(context.Background(), "cluster-a", metaV1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.Data["namespaces"]) != "team-a" {
		t.Errorf("ArgoCD `namespaces` was destroyed: %q", got.Data["namespaces"])
	}
	if string(got.Data["project"]) != "infra" {
		t.Errorf("ArgoCD `project` was destroyed: %q", got.Data["project"])
	}
	if got.Annotations["managed-by"] != "argocd.argoproj.io" {
		t.Errorf("annotations were destroyed: %v", got.Annotations)
	}
	if got.Labels["unrelated"] != "keepme" {
		t.Errorf("foreign label was destroyed: %v", got.Labels)
	}
	if string(got.Data["server"]) != "https://new" {
		t.Errorf("server was not updated: %q", got.Data["server"])
	}
}

// The vcluster decoy: `vc-*` matches vc-config-<name> first, and it holds no
// kubeconfig. The namespace must not be skipped because of it.
func TestFindKubeconfigSecretSkipsDecoy(t *testing.T) {
	decoy := &coreV1.Secret{
		ObjectMeta: metaV1.ObjectMeta{Name: "vc-config-x", Namespace: "ns"},
		Data:       map[string][]byte{"config.yaml": []byte("not a kubeconfig")},
	}
	wanted := &coreV1.Secret{
		ObjectMeta: metaV1.ObjectMeta{Name: "vc-x", Namespace: "ns"},
		Data:       map[string][]byte{"config": []byte(vclusterKubeconfig)},
	}
	r, _ := newTestRegistrar(decoy, wanted)
	r.cfg.Providers = mustPresets(t, "vcluster")

	got, err := r.findKubeconfigCandidates(context.Background(), "ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one candidate, got %d: %+v", len(got), got)
	}
	if got[0].secret.Name != "vc-x" {
		t.Errorf("picked %q, want vc-x (these names put the decoy first)", got[0].secret.Name)
	}
}

func secretWith(ns, name, key, body string) *coreV1.Secret {
	return &coreV1.Secret{
		ObjectMeta: metaV1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{key: []byte(body)},
	}
}

// Two provisioners under ONE registrar is the whole point of 0.3.0: before it,
// a second shape needed a second Deployment, and two Deployments sharing a
// managed-by value garbage collect each other's Secrets.
func TestMultipleProvidersRegisterSideBySide(t *testing.T) {
	r, c := newTestRegistrar(
		managedNS("k3k-a", "a"), kubeconfigSecret("k3k-a", "k3k-a-kubeconfig"),
		managedNS("tenant-b", "b"), secretWith("tenant-b", "b-admin-kubeconfig", "admin.conf", kamajiKubeconfig),
	)
	r.cfg.Providers = mustPresets(t, "k3k", "kamaji")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for cluster, wantProvider := range map[string]string{"a": "k3k", "b": "kamaji"} {
		got, err := c.CoreV1().Secrets("argocd").Get(context.Background(), secretName(cluster), metaV1.GetOptions{})
		if err != nil {
			t.Fatalf("cluster-%s was not registered: %v", cluster, err)
		}
		if got.Labels[ProviderLabel(testPrefix)] != wantProvider {
			t.Errorf("cluster-%s provider label = %q, want %q",
				cluster, got.Labels[ProviderLabel(testPrefix)], wantProvider)
		}
	}
}

// Deleting one child must collect exactly that registration. Cross-provider GC
// is the way this change could destroy data, so it is asserted directly.
func TestGarbageCollectionIsPerClusterAcrossProviders(t *testing.T) {
	r, c := newTestRegistrar(
		managedNS("k3k-a", "a"), kubeconfigSecret("k3k-a", "k3k-a-kubeconfig"), registeredSecret("a", "k3k-a"),
		// b's namespace is gone; only its registration remains.
		registeredSecret("b", "tenant-b"),
	)
	r.cfg.Providers = mustPresets(t, "k3k", "kamaji")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("cluster-a was collected although its namespace is still present")
	}
	if secretExists(t, c, "cluster-b") {
		t.Error("cluster-b should have been collected once its namespace was gone")
	}
}

// Driving Kamaji through its Cluster API control-plane provider produces TWO
// Secrets for one physical cluster: Kamaji's own `<tcp>-admin-kubeconfig`, and a
// CAPI-shaped `<cluster>-kubeconfig` copied from it. With both presets enabled
// they both match, and registering each would put one cluster into ArgoCD twice.
func TestKamajiViaCAPIRegistersOnce(t *testing.T) {
	r, c := newTestRegistrar(
		managedNS("tenant-b", "b"),
		secretWith("tenant-b", "b-admin-kubeconfig", "admin.conf", kamajiKubeconfig),
		secretWith("tenant-b", "b-kubeconfig", "value", capiKubeconfig),
	)
	r.cfg.Providers = mustPresets(t, "kamaji", "capi")

	children, _, err := r.discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("one physical cluster produced %d registrations: %+v", len(children), children)
	}
	if children[0].provider != "kamaji" {
		t.Errorf("provider = %q, want kamaji (declared first)", children[0].provider)
	}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	list, err := c.CoreV1().Secrets("argocd").List(context.Background(), metaV1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected exactly one cluster Secret, got %d", len(list.Items))
	}
}

// A Secret can match the shape and still be unusable. CAPA's EKS path writes
// `<cluster>-user-kubeconfig` with an exec credential, which satisfies the CAPI
// glob and the `value` key but cannot be copied into an ArgoCD Secret.
//
// In the real EKS layout `c-kubeconfig` happens to sort before
// `c-user-kubeconfig`, so picking the first match would work by luck. The unusable
// Secret here is named to sort FIRST, so this passes only if a failed parse
// genuinely falls through to the next candidate.
func TestPrefersUsableCandidateOverExecCredential(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS("capi-c", "c"),
		secretWith("capi-c", "aaa-kubeconfig", "value", execKubeconfig),
		secretWith("capi-c", "c-kubeconfig", "value", capiKubeconfig),
	)
	r.cfg.Providers = mustPresets(t, "capi")

	children, _, err := r.discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("expected one registration, got %d: %+v", len(children), children)
	}
	if children[0].server != "https://192.168.1.196:6443" {
		t.Errorf("registered the exec-credential kubeconfig: server = %q", children[0].server)
	}
}

// A k3k Cluster reports ProvisioningFailed with `invalid character '<'` for
// roughly its first ninety seconds, so a Secret can exist holding content that
// does not parse. That window must never look like a deletion.
func TestUnparseableKubeconfigDoesNotDeregister(t *testing.T) {
	r, c := newTestRegistrar(
		managedNS("k3k-a", "a"),
		secretWith("k3k-a", "k3k-a-kubeconfig", "kubeconfig.yaml", "<html>not a kubeconfig</html>"),
		registeredSecret("a", "k3k-a"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Logf("reconcile reported: %v", err)
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("a half-written kubeconfig deregistered a live cluster")
	}
}

// One namespace stuck without a usable kubeconfig used to disable garbage
// collection for the entire fleet, so every other cluster's dead registration
// stayed in ArgoCD indefinitely.
func TestUnresolvedNamespaceDoesNotBlockCollectionOfOthers(t *testing.T) {
	r, c := newTestRegistrar(
		// Permanently stuck: matches the glob, never parses.
		managedNS("k3k-stuck", "stuck"),
		secretWith("k3k-stuck", "k3k-stuck-kubeconfig", "kubeconfig.yaml", "<html>"),
		registeredSecret("stuck", "k3k-stuck"),
		// Genuinely gone, and unrelated to the stuck one.
		registeredSecret("gone", "k3k-gone"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Logf("reconcile reported: %v", err)
	}
	if !secretExists(t, c, "cluster-stuck") {
		t.Error("the stuck cluster must be exempt from GC, not collected")
	}
	if secretExists(t, c, "cluster-gone") {
		t.Error("an unrelated stuck namespace blocked collection of a genuinely deleted cluster")
	}
}

func TestConfigValidate(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"empty target namespace": func(c *Config) { c.TargetNamespace = "" },
		"empty managed-by":       func(c *Config) { c.ManagedByValue = "" },
		"no providers":           func(c *Config) { c.Providers = nil },
		"empty provider name":    func(c *Config) { c.Providers[0].Name = "" },
		"empty secret keys":      func(c *Config) { c.Providers[0].SecretKeys = nil },
		"empty secret key":       func(c *Config) { c.Providers[0].SecretKeys = []string{""} },
		// The code has always checked this; the test never did.
		"empty pattern": func(c *Config) { c.Providers[0].SecretNamePattern = "" },
		"bad glob":      func(c *Config) { c.Providers[0].SecretNamePattern = "k3k-[" },
		"duplicate provider names": func(c *Config) {
			c.Providers = append(c.Providers, c.Providers[0])
		},
		// The name is written verbatim as a label value on every cluster Secret
		// this provider matches. Accepted here, it would be rejected by the
		// apiserver on every apply, forever, per cluster.
		"provider name is not a valid label value": func(c *Config) {
			c.Providers[0].Name = "my tool"
		},
		"provider name too long for a label value": func(c *Config) {
			c.Providers[0].Name = strings.Repeat("a", 64)
		},
		"prefix without slash": func(c *Config) { c.LabelPrefix = "example.com" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("expected a validation error, got nil")
			}
		})
	}
	if err := testConfig().Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

// A cluster name that cannot form a DNS-1123 Secret name would be rejected by the
// apiserver on every pass forever.
func TestDiscoverSkipsInvalidClusterNames(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS("k3k-bad", "Prod_Cluster"),
		kubeconfigSecret("k3k-bad", "k3k-bad-kubeconfig"),
	)
	children, unresolved, err := r.discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(children) != 0 {
		t.Errorf("expected the invalid name to be skipped, got %+v", children)
	}
	if !unresolved["k3k-bad"] {
		t.Error("a skipped namespace must be recorded as unresolved so its registration is exempt from GC")
	}
}
