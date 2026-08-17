package registrar

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	coreV1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testPrefix = "example.com/"
	// Shared by both test files: it is the ownership marker the registrar
	// discovers namespaces by and garbage collects on.
	testManagedBy = "cluster-registrar"
	testTargetNS  = "argocd"
	// testSourceNS is the source namespace used by the ownership tests.
	testSourceNS = "k3k-src"
	// keyConfig is the ArgoCD cluster Secret's credential key.
	keyConfig = "config"
	// testServer is a placeholder endpoint for cases where the address itself is
	// not what is being asserted.
	testServer = "https://x"
	// resourceSecrets names the resource in fake-clientset reactors.
	resourceSecrets = "secrets"
	// testHour spaces fixture namespaces apart when age decides a contested name.
	testHour = time.Hour

	// Kamaji's two kubeconfig keys. Named because the multi-key tests repeat them
	// and because which of the two wins is itself the assertion.
	keyAdminConf = "admin.conf"
	keyAdminSvc  = "admin.svc"
	// testNS is the source namespace most fixtures use.
	testNS = "k3k-a"
	// k3kServer is the endpoint inside the k3k kubeconfig fixture.
	k3kServer = "https://192.168.1.192"
	// testProject is a propagated label value, not ArgoCD's project data key.
	testProject = "platform"
)

func testConfig() Config {
	k3k, _ := Preset("k3k")
	return Config{
		TargetNamespace: testTargetNS,
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
		UID:  types.UID("uid-" + name),
		Labels: map[string]string{
			ManagedByLabel(testPrefix): testManagedBy,
			ClusterLabel(testPrefix):   cluster,
		},
	}}
}

// managedNSAt is managedNS with a creation timestamp, which is what decides a
// contested cluster name.
func managedNSAt(name, cluster string, age time.Duration) *coreV1.Namespace {
	ns := managedNS(name, cluster)
	ns.CreationTimestamp = metaV1.NewTime(time.Now().Add(-age))
	return ns
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
			Namespace: testTargetNS,
			Labels: map[string]string{
				argoSecretTypeLabel:              argoSecretTypeValue,
				ManagedByLabel(testPrefix):       testManagedBy,
				ClusterLabel(testPrefix):         cluster,
				SourceNamespaceLabel(testPrefix): srcNS,
			},
		},
		Data: map[string][]byte{"name": []byte(cluster), "server": []byte(testServer), "config": []byte("{}")},
	}
}

func secretExists(t *testing.T, c *fake.Clientset, name string) bool {
	t.Helper()
	_, err := c.CoreV1().Secrets(testTargetNS).Get(context.Background(), name, metaV1.GetOptions{})
	if err != nil && !apiErrors.IsNotFound(err) {
		t.Fatalf("unexpected error: %v", err)
	}
	return err == nil
}

// getSecret fetches a cluster Secret that the test requires to exist.
func getSecret(t *testing.T, c *fake.Clientset, name string) *coreV1.Secret {
	t.Helper()
	s, err := c.CoreV1().Secrets(testTargetNS).Get(context.Background(), name, metaV1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return s
}

// discoverNS fetches a namespace and evaluates it, which is what ReconcileOne
// does between proving the namespace exists and applying the result.
func discoverNS(t *testing.T, r *Registrar, name string) (child, bool) {
	t.Helper()
	ns, err := r.client.CoreV1().Namespaces().Get(context.Background(), name, metaV1.GetOptions{})
	if err != nil {
		t.Fatalf("get namespace %s: %v", name, err)
	}
	return r.discoverOne(context.Background(), ns)
}

// The catastrophe this guard exists for: a transient API error must never be
// read as "the cluster was deleted". Before the fix, one failing List emptied
// `desired` and collect() deregistered the whole fleet, while logging the
// failure as the reassuring "no kubeconfig secret yet".
func TestTransientSecretListErrorDoesNotDeregisterAnything(t *testing.T) {
	r, c := newTestRegistrar(
		managedNS(testNS, "a"), kubeconfigSecret(testNS, "k3k-a-kubeconfig"), registeredSecret("a", testNS),
		managedNS("k3k-b", "b"), kubeconfigSecret("k3k-b", "k3k-b-kubeconfig"), registeredSecret("b", "k3k-b"),
	)
	c.PrependReactor("list", resourceSecrets, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() != testTargetNS {
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
		Name:   testNS,
		Labels: map[string]string{ManagedByLabel(testPrefix): testManagedBy},
	}}
	r, c := newTestRegistrar(unlabelled, registeredSecret("a", testNS))

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("cluster-a deleted while its source namespace still exists")
	}
}

func TestCollectDeletesWhenNamespaceIsActuallyGone(t *testing.T) {
	r, c := newTestRegistrar(registeredSecret("gone", "k3k-gone"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
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
		Namespace: testTargetNS,
		Labels:    map[string]string{argoSecretTypeLabel: argoSecretTypeValue},
	}}
	r, c := newTestRegistrar(hand)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-handmade") {
		t.Error("a hand-registered cluster Secret was deleted")
	}
}

// One undeletable Secret must not stall GC for every other cluster.
func TestCollectContinuesPastDeleteErrors(t *testing.T) {
	r, c := newTestRegistrar(registeredSecret("a", testNS), registeredSecret("b", "k3k-b"))
	c.PrependReactor("delete", resourceSecrets, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.DeleteAction).GetName() == "cluster-a" {
			return true, nil, apiErrors.NewForbidden(schema.GroupResource{Resource: resourceSecrets}, "cluster-a", io.ErrUnexpectedEOF)
		}
		return false, nil, nil
	})

	if err := r.Reconcile(context.Background()); err == nil {
		t.Error("expected the forbidden delete to be reported")
	}
	if secretExists(t, c, "cluster-b") {
		t.Error("cluster-b was not deleted; one failure stalled the whole GC pass")
	}
}

// Update replaces the whole object, so a freshly-built Secret would silently drop
// ArgoCD's own fields and any operator-set annotations.
func TestApplyPreservesForeignFieldsAndAnnotations(t *testing.T) {
	existing := registeredSecret("a", testNS)
	existing.Data["namespaces"] = []byte("team-a")
	existing.Data["project"] = []byte("infra")
	existing.Annotations = map[string]string{"managed-by": "argocd.argoproj.io"}
	existing.Labels["unrelated"] = "keepme"

	r, c := newTestRegistrar(existing)
	err := r.apply(context.Background(), child{
		cluster: "a", namespace: testNS,
		server: "https://new", config: `{"tlsClientConfig":{"insecure":false}}`,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	got, err := c.CoreV1().Secrets(testTargetNS).Get(context.Background(), "cluster-a", metaV1.GetOptions{})
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

	got, _, err := r.findKubeconfigCandidates(context.Background(), "ns")
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
		managedNS(testNS, "a"), kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
		managedNS("tenant-b", "b"), secretWith("tenant-b", "b-admin-kubeconfig", keyAdminConf, kamajiKubeconfig),
	)
	r.cfg.Providers = mustPresets(t, "k3k", "kamaji")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for cluster, wantProvider := range map[string]string{"a": "k3k", "b": "kamaji"} {
		got, err := c.CoreV1().Secrets(testTargetNS).Get(context.Background(), secretName(cluster), metaV1.GetOptions{})
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
		managedNS(testNS, "a"), kubeconfigSecret(testNS, "k3k-a-kubeconfig"), registeredSecret("a", testNS),
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
//
// What this pins is PROVIDER PRECEDENCE. Read it as a guard on the candidate
// count and you will overrate it: capi's `value` key is absent from the Kamaji
// Secret, so no change to how keys become candidates can make it fail. The
// per-Secret `claimed` rule is what it actually holds in place.
func TestKamajiViaCAPIRegistersOnce(t *testing.T) {
	r, c := newTestRegistrar(
		managedNS("tenant-b", "b"),
		secretWith("tenant-b", "b-admin-kubeconfig", keyAdminConf, kamajiKubeconfig),
		secretWith("tenant-b", "b-kubeconfig", "value", capiKubeconfig),
	)
	r.cfg.Providers = mustPresets(t, "kamaji", "capi")

	ch, ok := discoverNS(t, r, "tenant-b")
	if !ok {
		t.Fatal("one physical cluster produced no registration")
	}
	if ch.provider != "kamaji" {
		t.Errorf("provider = %q, want kamaji (declared first)", ch.provider)
	}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	list, err := c.CoreV1().Secrets(testTargetNS).List(context.Background(), metaV1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected exactly one cluster Secret, got %d", len(list.Items))
	}
}

// A Secret can match the shape and still be unusable. CAPA's EKS path writes
// `<cluster>-user-kubeconfig` with an exec credential, which satisfies the CAPI
// glob and the `value` key; `capi` does not allow translating it.
//
// In the real EKS layout `c-kubeconfig` happens to sort before
// `c-user-kubeconfig`, so picking the first match would work by luck. The unusable
// Secret here is named to sort FIRST, so this passes only if a failed parse
// genuinely falls through to the next candidate.
func TestPrefersUsableCandidateOverUntranslatableExec(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS("capi-c", "c"),
		secretWith("capi-c", "aaa-kubeconfig", "value", execKubeconfig),
		secretWith("capi-c", "c-kubeconfig", "value", capiKubeconfig),
	)
	// `capi` does not allow exec, so the exec candidate is refused and the next
	// one is tried. It loses because this provider disallows translation, not
	// because an exec credential is inherently uncopyable.
	r.cfg.Providers = mustPresets(t, "capi")

	ch, ok := discoverNS(t, r, "capi-c")
	if !ok {
		t.Fatal("expected one registration, got none")
	}
	if ch.server != "https://192.168.1.196:6443" {
		t.Errorf("registered the exec-credential kubeconfig: server = %q", ch.server)
	}
}

// A k3k Cluster reports ProvisioningFailed with `invalid character '<'` for
// roughly its first ninety seconds, so a Secret can exist holding content that
// does not parse. That window must never look like a deletion.
func TestUnparseableKubeconfigDoesNotDeregister(t *testing.T) {
	r, c := newTestRegistrar(
		managedNS(testNS, "a"),
		secretWith(testNS, "k3k-a-kubeconfig", "kubeconfig.yaml", "<html>not a kubeconfig</html>"),
		registeredSecret("a", testNS),
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

// The escalation this release closes. apply() used to Get the Secret, overwrite it
// and relabel it as ours regardless of who wrote it, so a tenant able to label
// their own namespace could repoint a hand-registered cluster at their own API
// server. TestCollectNeverTouchesUnlabelledSecrets covers only collect() and gave
// false reassurance about this path.
func TestApplyRefusesUnownedSecret(t *testing.T) {
	hand := &coreV1.Secret{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      "cluster-prod",
			Namespace: testTargetNS,
			Labels:    map[string]string{argoSecretTypeLabel: argoSecretTypeValue},
		},
		Data: map[string][]byte{
			"name":    []byte("prod"),
			"server":  []byte("https://real-prod:6443"),
			keyConfig: []byte(`{"bearerToken":"real"}`),
		},
	}
	r, c := newTestRegistrar(hand)

	err := r.apply(context.Background(), child{
		cluster: "prod", namespace: "tenant-evil", namespaceUID: "uid-tenant-evil",
		server: "https://attacker:6443", config: `{"bearerToken":"stolen"}`,
	})
	var conflict *conflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("apply returned %v, want a conflictError", err)
	}

	got := getSecret(t, c, "cluster-prod")
	if string(got.Data["server"]) != "https://real-prod:6443" {
		t.Errorf("server was repointed to %q", got.Data["server"])
	}
	if string(got.Data[keyConfig]) != `{"bearerToken":"real"}` {
		t.Errorf("credentials were replaced: %q", got.Data[keyConfig])
	}
	if v, ok := got.Labels[ManagedByLabel(testPrefix)]; ok {
		t.Errorf("a hand-registered Secret was relabelled as ours (%s=%q)",
			ManagedByLabel(testPrefix), v)
	}
	if v, ok := got.Labels[SourceNamespaceLabel(testPrefix)]; ok {
		t.Errorf("source-namespace was stamped at the attacker (%q), making it GC-eligible", v)
	}
}

// The same refusal between two namespaces that are both legitimately managed.
func TestApplyRefusesSecretOwnedByAnotherNamespace(t *testing.T) {
	existing := registeredSecret("a", testSourceNS)
	existing.Data["server"] = []byte("https://incumbent:6443")
	r, c := newTestRegistrar(existing)

	err := r.apply(context.Background(), child{
		cluster: "a", namespace: "k3k-evil", namespaceUID: "uid-k3k-evil",
		server: "https://challenger:6443", config: "{}",
	})
	var conflict *conflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("apply returned %v, want a conflictError", err)
	}

	got := getSecret(t, c, "cluster-a")
	if string(got.Data["server"]) != "https://incumbent:6443" {
		t.Errorf("the incumbent registration was repointed to %q", got.Data["server"])
	}
	if got.Labels[SourceNamespaceLabel(testPrefix)] != testSourceNS {
		t.Errorf("ownership moved to %q", got.Labels[SourceNamespaceLabel(testPrefix)])
	}
}

// An owned Secret with no recorded source is adoptable, but only by the cluster it
// already names. Stripping the label must not turn into a way to claim someone
// else's registration.
func TestApplyAdoptsSecretWithMatchingClusterLabel(t *testing.T) {
	existing := registeredSecret("a", testSourceNS)
	delete(existing.Labels, SourceNamespaceLabel(testPrefix))
	r, c := newTestRegistrar(existing)

	if err := r.apply(context.Background(), child{
		cluster: "a", namespace: testSourceNS, namespaceUID: "uid-k3k-a",
		server: testServer, config: "{}",
	}); err != nil {
		t.Fatalf("apply refused to adopt its own registration: %v", err)
	}

	got := getSecret(t, c, "cluster-a")
	if got.Labels[SourceNamespaceLabel(testPrefix)] != testSourceNS {
		t.Errorf("adoption did not record the source namespace: %v", got.Labels)
	}
}

// Adoption is bounded by the cluster label. A Secret at the name `cluster-a` whose
// own labels say it belongs to a different cluster is not an orphan of ours, so
// stripping source-namespace must not make it claimable.
func TestApplyRefusesAdoptionWhenClusterLabelDiffers(t *testing.T) {
	existing := registeredSecret("a", testSourceNS)
	delete(existing.Labels, SourceNamespaceLabel(testPrefix))
	existing.Labels[ClusterLabel(testPrefix)] = "somethingelse"
	r, c := newTestRegistrar(existing)

	err := r.apply(context.Background(), child{
		cluster: "a", namespace: testSourceNS, namespaceUID: "uid-k3k-a",
		server: "https://new", config: "{}",
	})
	var conflict *conflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("apply returned %v, want a conflictError", err)
	}

	got := getSecret(t, c, "cluster-a")
	if string(got.Data["server"]) == "https://new" {
		t.Error("the Secret was adopted and overwritten anyway")
	}
}

// Nobody holds the name, so incumbency cannot settle it. The oldest namespace wins.
func TestOldestNamespaceWinsAContestedName(t *testing.T) {
	// Deliberately named so that the OLDER namespace sorts last: if alphabetical
	// order were deciding, this test would fail.
	r, c := newTestRegistrar(
		managedNSAt("zzz-older", "a", 2*time.Hour), kubeconfigSecret("zzz-older", "k3k-zzz-kubeconfig"),
		managedNSAt("aaa-newer", "a", 1*time.Hour), kubeconfigSecret("aaa-newer", "k3k-aaa-kubeconfig"),
		// Oldest of all, and first alphabetically, but claims a different cluster.
		// If the claimant lookup ignored its label selector this would win.
		managedNSAt("aaa-decoy", "other", 5*time.Hour), kubeconfigSecret("aaa-decoy", "k3k-decoy-kubeconfig"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("a contested name must not fail the pass: %v", err)
	}

	got := getSecret(t, c, "cluster-a")
	if src := got.Labels[SourceNamespaceLabel(testPrefix)]; src != "zzz-older" {
		t.Errorf("cluster-a went to %q, want the oldest claimant zzz-older", src)
	}
	if !secretExists(t, c, "cluster-other") {
		t.Error("the uncontested cluster was not registered")
	}
}

// The property the whole design rests on: incumbency outranks the tiebreak. An
// older namespace appearing later must never evict a live registration, or the
// tiebreak becomes an eviction primitive rather than an allocation rule.
// The challenger is named to sort LAST, so that without the ownership check it
// would be the final writer and would win. Passing by luck of iteration order is
// exactly what this test has to rule out.
func TestIncumbentKeepsNameAgainstAnOlderChallenger(t *testing.T) {
	// The two claimants carry DIFFERENT kubeconfigs, so `server` shows which one
	// actually got written rather than merely which label survived.
	r, c := newTestRegistrar(
		managedNSAt("aaa-incumbent", "a", 1*time.Hour),
		kubeconfigSecret("aaa-incumbent", "k3k-inc-kubeconfig"),
		managedNSAt("zzz-challenger", "a", 9*time.Hour),
		secretWith("zzz-challenger", "chal-kubeconfig", "value", capiKubeconfig),
		registeredSecret("a", "aaa-incumbent"),
	)
	r.cfg.Providers = mustPresets(t, "k3k", "capi")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := getSecret(t, c, "cluster-a")
	if src := got.Labels[SourceNamespaceLabel(testPrefix)]; src != "aaa-incumbent" {
		t.Errorf("an older challenger evicted the incumbent: source-namespace = %q", src)
	}
	if got.Labels[ProviderLabel(testPrefix)] != "k3k" {
		t.Errorf("provider = %q, want the incumbent's k3k", got.Labels[ProviderLabel(testPrefix)])
	}
	if s := string(got.Data["server"]); s != k3kServer {
		t.Errorf("the registration was repointed at the challenger's endpoint: %q", s)
	}
}

// A refused claimant vouches for nothing. If it still marked the name live, then
// once the WINNER's namespace was deleted the registration would be stranded
// forever: the loser stays refused, and GC keeps seeing the name as claimed.
func TestConflictedLoserDoesNotStrandTheWinnersSecret(t *testing.T) {
	// k3k-winner is gone; only its registration remains.
	r, c := newTestRegistrar(
		registeredSecret("a", "k3k-winner"),
		managedNSAt("k3k-loser", "a", 1*time.Hour), kubeconfigSecret("k3k-loser", "k3k-loser-kubeconfig"),
	)

	// Pass one refuses the loser, then collects the dead winner's registration.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	// Pass two: the name is free, so the loser takes it. Two passes because apply
	// runs before collect within a pass.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	got := getSecret(t, c, "cluster-a")
	if src := got.Labels[SourceNamespaceLabel(testPrefix)]; src != "k3k-loser" {
		t.Errorf("source-namespace = %q, want k3k-loser", src)
	}
}

// Someone wrote the Secret between our Get and our Create. Benign, resolved by
// incumbency next pass -- and the tiebreak that keeps this safe once more than one
// worker reconciles at a time.
func TestCreateRaceReturnsConflict(t *testing.T) {
	r, c := newTestRegistrar(managedNS(testSourceNS, "a"), kubeconfigSecret(testSourceNS, "k3k-a-kubeconfig"))
	c.PrependReactor("create", resourceSecrets, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apiErrors.NewAlreadyExists(
			schema.GroupResource{Resource: resourceSecrets}, "cluster-a")
	})

	err := r.apply(context.Background(), child{
		cluster: "a", namespace: testSourceNS, namespaceUID: "uid-k3k-a",
		server: testServer, config: "{}",
	})
	var conflict *conflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("apply returned %v, want a conflictError", err)
	}
}

// --dry-run exists to be run against an existing cluster before committing to it.
// A takeover it would have performed is exactly what it must report, so the
// ownership check has to sit ahead of the dry-run return.
func TestDryRunReportsConflictAndWritesNothing(t *testing.T) {
	existing := registeredSecret("a", testSourceNS)
	r, c := newTestRegistrar(existing)
	r.cfg.DryRun = true

	err := r.apply(context.Background(), child{
		cluster: "a", namespace: "k3k-evil", namespaceUID: "uid-k3k-evil",
		server: "https://attacker", config: "{}",
	})
	var conflict *conflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("dry-run reported %v, want a conflictError", err)
	}

	got := getSecret(t, c, "cluster-a")
	if got.Labels[SourceNamespaceLabel(testPrefix)] != testSourceNS {
		t.Error("dry-run mutated the Secret")
	}
}

// A conflict alone must not fail the pass; a genuine write failure must.
func TestReconcileReturnsNilOnConflictButErrorsOnRealFailure(t *testing.T) {
	newFixture := func() (*Registrar, *fake.Clientset) {
		return newTestRegistrar(
			registeredSecret("a", testSourceNS),
			managedNS("k3k-evil", "a"), kubeconfigSecret("k3k-evil", "k3k-evil-kubeconfig"),
			managedNS("k3k-b", "b"), kubeconfigSecret("k3k-b", "k3k-b-kubeconfig"),
		)
	}

	r, _ := newFixture()
	if err := r.Reconcile(context.Background()); err != nil {
		t.Errorf("a contested name failed the pass: %v", err)
	}

	r, c := newFixture()
	c.PrependReactor("create", resourceSecrets, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apiErrors.NewForbidden(
			schema.GroupResource{Resource: resourceSecrets}, "cluster-b", io.ErrUnexpectedEOF)
	})
	if err := r.Reconcile(context.Background()); err == nil {
		t.Error("a forbidden write was not reported")
	}
}

// Renaming a cluster used to strand the old registration forever: its source
// namespace still exists, so collect refuses to delete and merely warns. That is
// not inert -- apply only runs over what was discovered, so the stale Secret is
// never rewritten and keeps working off a frozen kubeconfig, and ArgoCD picks
// between two registrations sharing a server URL nondeterministically.
func TestClusterRenameDemotesOldRegistration(t *testing.T) {
	r, c := newTestRegistrar(
		managedNS("k3k-x", "a2"), kubeconfigSecret("k3k-x", "k3k-x-kubeconfig"),
		registeredSecret("a", "k3k-x"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !secretExists(t, c, "cluster-a2") {
		t.Fatal("the renamed cluster was not registered")
	}
	old := getSecret(t, c, "cluster-a")
	if _, ok := old.Labels[argoSecretTypeLabel]; ok {
		t.Error("the superseded registration is still visible to ArgoCD")
	}
	if got := old.Labels[OrphanedSecretTypeLabel(testPrefix)]; got != argoSecretTypeValue {
		t.Errorf("secret-type was not parked: %q", got)
	}
	if got := old.Labels[SupersededByLabel(testPrefix)]; got != "cluster-a2" {
		t.Errorf("superseded-by = %q, want cluster-a2", got)
	}
	if old.Labels[StaleSinceLabel(testPrefix)] == "" {
		t.Error("stale-since was not stamped")
	}
	// A label value may not contain ':', which is why stale-since is not RFC3339.
	// The fake clientset does not validate, so assert it here or a real apiserver
	// would reject every demotion.
	for k, v := range old.Labels {
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			t.Errorf("label %s=%q is not a valid label value: %v", k, v, errs)
		}
	}
}

// The reason this demotes rather than deletes. A rename that gets reverted must
// come back whole, including everything ArgoCD and operators wrote into it.
func TestRevertedRenameRestoresDemotedRegistrationWithForeignData(t *testing.T) {
	existing := registeredSecret("a", "k3k-x")
	existing.Data["namespaces"] = []byte("team-a")
	existing.Data["project"] = []byte("infra")
	existing.Annotations = map[string]string{"managed-by": "argocd.argoproj.io"}
	existing.Labels["unrelated"] = "keepme"

	ns := managedNS("k3k-x", "a2")
	r, c := newTestRegistrar(ns, kubeconfigSecret("k3k-x", "k3k-x-kubeconfig"), existing)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile (renamed): %v", err)
	}
	if _, ok := getSecret(t, c, "cluster-a").Labels[argoSecretTypeLabel]; ok {
		t.Fatal("precondition: cluster-a should have been demoted")
	}

	// Revert the rename.
	ns.Labels[ClusterLabel(testPrefix)] = "a"
	if _, err := c.CoreV1().Namespaces().Update(context.Background(), ns, metaV1.UpdateOptions{}); err != nil {
		t.Fatalf("update namespace: %v", err)
	}
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile (reverted): %v", err)
	}

	got := getSecret(t, c, "cluster-a")
	if got.Labels[argoSecretTypeLabel] != argoSecretTypeValue {
		t.Error("the registration was not restored to ArgoCD's view")
	}
	for _, l := range []string{
		OrphanedSecretTypeLabel(testPrefix),
		SupersededByLabel(testPrefix),
		StaleSinceLabel(testPrefix),
	} {
		if v, ok := got.Labels[l]; ok {
			t.Errorf("demotion label %s=%q survived the restore", l, v)
		}
	}
	if string(got.Data["namespaces"]) != "team-a" || string(got.Data["project"]) != "infra" {
		t.Errorf("ArgoCD's own fields were lost: %q %q", got.Data["namespaces"], got.Data["project"])
	}
	if got.Annotations["managed-by"] != "argocd.argoproj.io" {
		t.Errorf("annotations were lost: %v", got.Annotations)
	}
	if got.Labels["unrelated"] != "keepme" {
		t.Errorf("foreign label was lost: %v", got.Labels)
	}
	if string(got.Data["server"]) != k3kServer {
		t.Errorf("credentials were not refreshed on restore: %q", got.Data["server"])
	}
}

// A demoted Secret keeps no ArgoCD label, so garbage collection must not select on
// one, or it would linger forever once its namespace finally went away.
func TestDemotedSecretIsStillCollectedWhenNamespaceGone(t *testing.T) {
	demoted := registeredSecret("a", "k3k-gone")
	delete(demoted.Labels, argoSecretTypeLabel)
	demoted.Labels[OrphanedSecretTypeLabel(testPrefix)] = argoSecretTypeValue
	demoted.Labels[SupersededByLabel(testPrefix)] = "cluster-a2"

	r, c := newTestRegistrar(demoted)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if secretExists(t, c, "cluster-a") {
		t.Error("a demoted registration was not collected once its namespace was gone")
	}
}

// Demotion happens once. Repeating it every interval would rewrite stale-since
// forever and churn the object.
func TestDemotionIsNotRepeated(t *testing.T) {
	r, c := newTestRegistrar(
		managedNS("k3k-x", "a2"), kubeconfigSecret("k3k-x", "k3k-x-kubeconfig"),
		registeredSecret("a", "k3k-x"),
	)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	first := getSecret(t, c, "cluster-a").Labels[StaleSinceLabel(testPrefix)]

	c.PrependReactor("update", resourceSecrets, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.UpdateAction).GetObject().(*coreV1.Secret).Name == "cluster-a" {
			t.Error("cluster-a was demoted a second time")
		}
		return false, nil, nil
	})
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if got := getSecret(t, c, "cluster-a").Labels[StaleSinceLabel(testPrefix)]; got != first {
		t.Errorf("stale-since was rewritten: %q then %q", first, got)
	}
}

// A terminating namespace is the one case that reaches collection having produced
// no registration WITHOUT being marked unevaluable. Its live Secret must be left
// exactly as it is: not re-registered, and above all not demoted. Demoting here
// would hide a working cluster moments before its namespace finished going away,
// and would leave it hidden for good if a finalizer then aborted the deletion.
func TestTerminatingNamespaceIsNeitherRegisteredNorCollected(t *testing.T) {
	ns := managedNS("k3k-x", "a")
	now := metaV1.Now()
	ns.DeletionTimestamp = &now
	ns.Finalizers = []string{"kubernetes.io/test"}

	r, c := newTestRegistrar(ns, kubeconfigSecret("k3k-x", "k3k-x-kubeconfig"),
		registeredSecret("a", "k3k-x"))

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := getSecret(t, c, "cluster-a")
	if got.Labels[argoSecretTypeLabel] != argoSecretTypeValue {
		t.Error("a terminating namespace demoted its own live registration")
	}
	for _, l := range []string{
		OrphanedSecretTypeLabel(testPrefix),
		SupersededByLabel(testPrefix),
		StaleSinceLabel(testPrefix),
	} {
		if _, ok := got.Labels[l]; ok {
			t.Errorf("registration was demoted while its namespace was terminating (%s)", l)
		}
	}
}

// The sweep has to revisit namespaces that no longer exist, or nothing would ever
// be collected. Their names are recoverable only from the registrations they left
// behind.
func TestSweepVisitsNamespacesKnownOnlyFromTheirRegistrations(t *testing.T) {
	r, _ := newTestRegistrar(registeredSecret("gone", "k3k-gone"), managedNS("k3k-live", "live"))

	keys, err := r.reconcileKeys(context.Background())
	if err != nil {
		t.Fatalf("reconcileKeys: %v", err)
	}
	want := map[string]bool{"k3k-gone": true, "k3k-live": true}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected key %q", k)
		}
	}
}

// An owned Secret whose source-namespace label was stripped matches no collection
// selector and routes to no key, so it is invisible unless something goes looking.
func TestAuditReportsSecretsThatCanNeverBeCollected(t *testing.T) {
	stranded := registeredSecret("a", testNS)
	delete(stranded.Labels, SourceNamespaceLabel(testPrefix))

	var buf strings.Builder
	r, c := newTestRegistrar(stranded)
	r.log = slog.New(slog.NewTextHandler(&buf, nil))

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-a") {
		t.Fatal("an unroutable Secret was deleted; it must never be, there is no proof")
	}
	if !strings.Contains(buf.String(), "records no source namespace") {
		t.Errorf("the audit said nothing about it:\n%s", buf.String())
	}
}

// Under a watch the gap between proving a namespace gone and deleting its
// registration is event latency plus a requeue, not microseconds. If the Secret is
// recreated in that window the delete must not land on the new object, which is
// what the UID precondition is for. The fake clientset does not enforce
// preconditions, so this asserts the request carries one.
func TestOrphanDeleteIsPreconditionedOnTheSecretUID(t *testing.T) {
	orphan := registeredSecret("gone", "k3k-gone")
	orphan.UID = types.UID("uid-cluster-gone")

	r, c := newTestRegistrar(orphan)
	var gotUID types.UID
	var sawDelete bool
	c.PrependReactor("delete", resourceSecrets, func(action k8stesting.Action) (bool, runtime.Object, error) {
		sawDelete = true
		if pre := action.(k8stesting.DeleteActionImpl).DeleteOptions.Preconditions; pre != nil && pre.UID != nil {
			gotUID = *pre.UID
		}
		return false, nil, nil
	})

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !sawDelete {
		t.Fatal("the orphaned registration was never deleted")
	}
	if gotUID != orphan.UID {
		t.Errorf("delete precondition UID = %q, want %q", gotUID, orphan.UID)
	}
}

// Deletion used to take up to a full interval to happen, which was often long
// enough for a human to notice a mistaken label edit. It is now near-instant, so
// there is an escape hatch.
func TestPruneDisabledSurvivesBothRemovalPaths(t *testing.T) {
	t.Run("deletion", func(t *testing.T) {
		pinned := registeredSecret("gone", "k3k-gone")
		pinned.Labels[PruneLabel(testPrefix)] = PruneDisabled
		r, c := newTestRegistrar(pinned)

		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if !secretExists(t, c, "cluster-gone") {
			t.Error("an opted-out registration was deleted when its namespace went away")
		}
	})

	t.Run("demotion", func(t *testing.T) {
		pinned := registeredSecret("a", "k3k-x")
		pinned.Labels[PruneLabel(testPrefix)] = PruneDisabled
		r, c := newTestRegistrar(managedNS("k3k-x", "a2"),
			kubeconfigSecret("k3k-x", "k3k-x-kubeconfig"), pinned)

		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		got := getSecret(t, c, "cluster-a")
		if got.Labels[argoSecretTypeLabel] != argoSecretTypeValue {
			t.Error("an opted-out registration was demoted on rename")
		}
	})
}

// A typo must fail safe towards normal collection, not silently pin a
// registration in place forever.
func TestPruneOnlyRecognisesDisabled(t *testing.T) {
	pinned := registeredSecret("gone", "k3k-gone")
	pinned.Labels[PruneLabel(testPrefix)] = "false"
	r, c := newTestRegistrar(pinned)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if secretExists(t, c, "cluster-gone") {
		t.Error(`prune="false" was treated as an opt-out; only "disabled" may be`)
	}
}

// The opt-out belongs to whoever owns the ArgoCD namespace. A tenant setting it
// on their own namespace must not be able to pin a registration that outlives
// their cluster.
func TestPruneCannotBePropagatedFromTheSourceNamespace(t *testing.T) {
	ns := managedNS("k3k-a", "a")
	ns.Labels[PruneLabel(testPrefix)] = PruneDisabled
	r, c := newTestRegistrar(ns, kubeconfigSecret(testNS, "k3k-a-kubeconfig"))

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if v, ok := getSecret(t, c, "cluster-a").Labels[PruneLabel(testPrefix)]; ok {
		t.Errorf("prune=%q was copied off the source namespace", v)
	}
}

// Namespace names are reusable and UIDs are not. Deleting and recreating a
// namespace -- which a GitOps re-apply does routinely -- leaves the predecessor's
// registration pointing at a destroyed API server, and every other path returns
// early rather than conclude anything about a namespace it could not evaluate.
func TestReplacedNamespaceDoesNotStrandItsPredecessorsRegistration(t *testing.T) {
	stale := registeredSecret("a", testNS)
	stale.Labels[SourceNamespaceUIDLabel(testPrefix)] = "uid-old"

	// The replacement exists but never becomes discoverable: no kubeconfig Secret.
	replacement := managedNS(testNS, "a")
	replacement.UID = "uid-new"

	r, c := newTestRegistrar(replacement, stale)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if secretExists(t, c, "cluster-a") {
		t.Error("a registration whose source namespace was replaced is still present, " +
			"pointing at a cluster that no longer exists")
	}
}

// Absence of the label is not a mismatch. Every Secret written before 0.4.0 lacks
// it, so reading absence as "replaced" would deregister a fleet on upgrade.
func TestRegistrationsWithoutARecordedUIDAreNeverCollectedAsStale(t *testing.T) {
	legacy := registeredSecret("a", testNS)
	if _, ok := legacy.Labels[SourceNamespaceUIDLabel(testPrefix)]; ok {
		t.Fatal("precondition: the fixture must not carry a recorded UID")
	}
	ns := managedNS(testNS, "a")
	ns.UID = "uid-whatever"

	r, c := newTestRegistrar(ns, legacy)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("a pre-0.4.0 registration was collected because it records no UID")
	}
}

// A matching UID is the ordinary case and must be untouched, including on the
// paths that skip discovery entirely.
func TestMatchingUIDIsLeftAlone(t *testing.T) {
	ns := managedNS(testNS, "a")
	current := registeredSecret("a", testNS)
	current.Labels[SourceNamespaceUIDLabel(testPrefix)] = string(ns.UID)

	r, c := newTestRegistrar(ns, current) // no kubeconfig Secret: discovery fails
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("a registration whose source namespace is unchanged was collected")
	}
}

// The opt-out has to hold on this path too, or pinning a registration protects it
// from one collector and not the other.
func TestPruneDisabledSurvivesAReplacedNamespace(t *testing.T) {
	pinned := registeredSecret("a", testNS)
	pinned.Labels[SourceNamespaceUIDLabel(testPrefix)] = "uid-old"
	pinned.Labels[PruneLabel(testPrefix)] = PruneDisabled

	ns := managedNS(testNS, "a")
	ns.UID = "uid-new"

	r, c := newTestRegistrar(ns, pinned)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("an opted-out registration was collected as stale")
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
		// Under this prefix a source namespace could propagate
		// argocd.argoproj.io/secret-type, which is not a reserved suffix, and the
		// propagated labels are copied last so it would win.
		// Not "use the default": with no prefix, propagatedLabels matches every
		// label and the reserved list matches none of the keys actually written.
		"empty prefix": func(c *Config) { c.LabelPrefix = "" },
		"prefix that ArgoCD's own key falls under": func(c *Config) {
			c.LabelPrefix = "argocd.argoproj.io/"
		},
		// A partial prefix reaches every key under the domain, not just
		// secret-type, and what it would reach matters: skip-reconcile stops
		// reconciliation of every Application targeting the cluster.
		"prefix shorter than the ArgoCD domain": func(c *Config) {
			c.LabelPrefix = "argocd.argoproj.i"
		},
		// Negative would expire every demoted registration on sight, since
		// time.Since is always greater than a negative duration.
		"negative demoted TTL": func(c *Config) { c.DemotedTTL = -time.Second },
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
	if _, ok := discoverNS(t, r, "k3k-bad"); ok {
		t.Error("expected the invalid cluster name to be skipped")
	}
}

// secretWithKeys builds a Secret carrying several keys at once, which is the
// shape Kamaji actually writes and which no fixture covered before.
func secretWithKeys(ns, name string, kv map[string]string) *coreV1.Secret {
	data := make(map[string][]byte, len(kv))
	for k, v := range kv {
		data[k] = []byte(v)
	}
	return &coreV1.Secret{
		ObjectMeta: metaV1.ObjectMeta{Name: name, Namespace: ns},
		Data:       data,
	}
}

// Every key a provider declares that is actually present becomes its own
// candidate, in declared order.
//
// Stopping at the first present key made Provider.SecretKeys' "tried in order"
// a claim the code did not honour: with both Kamaji keys present there was only
// ever one candidate, so the second could not be reached however broken the
// first was.
func TestOneSecretYieldsOneCandidatePerPresentKey(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS("tenant-c", "c"),
		secretWithKeys("tenant-c", "c-admin-kubeconfig", map[string]string{
			keyAdminConf: kamajiKubeconfig,
			keyAdminSvc:  kamajiSvcKubeconfig,
		}),
	)
	r.cfg.Providers = mustPresets(t, "kamaji")

	got, _, err := r.findKubeconfigCandidates(context.Background(), "tenant-c")
	if err != nil {
		t.Fatalf("find candidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want one per present key", len(got))
	}
	// Declared order, not map order: Secret.Data is a map, so iterating it rather
	// than SecretKeys would pass or fail at random.
	if got[0].key != keyAdminConf || got[1].key != keyAdminSvc {
		t.Errorf("keys = %q then %q, want admin.conf then admin.svc (the declared order)",
			got[0].key, got[1].key)
	}
	if got[0].secret != got[1].secret {
		t.Error("the two candidates should share one Secret, not copy it")
	}
}

// An unusable first key must fall through to the next, exactly as an unusable
// first Secret already does.
//
// This is the bug B1 names: Kamaji writes admin.conf and admin.svc together, so
// a half-written admin.conf left the namespace unregistered indefinitely even
// though a perfectly good kubeconfig sat beside it.
func TestEveryPresentKeyOnOneSecretIsTriedInOrder(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS("tenant-d", "d"),
		secretWithKeys("tenant-d", "d-admin-kubeconfig", map[string]string{
			keyAdminConf: "<html>not a kubeconfig</html>",
			keyAdminSvc:  kamajiSvcKubeconfig,
		}),
	)
	r.cfg.Providers = mustPresets(t, "kamaji")

	ch, ok := discoverNS(t, r, "tenant-d")
	if !ok {
		t.Fatal("a usable admin.svc beside a broken admin.conf produced no registration")
	}
	// The exact address, not merely "no error": asserting the fallthrough reached
	// the SECOND key is the whole point, and the two fixtures differ only here.
	if ch.server != "https://tenant-00.kamaji.svc:6443" {
		t.Errorf("server = %q, want admin.svc's address", ch.server)
	}
}

// Several candidates, still one registration.
//
// Candidate count and registration count are different things: discoverOne stops
// at the first candidate that parses. Both keys are valid here and point at
// DIFFERENT servers, so a bug that kept going would be visible as the wrong
// address rather than as a second Secret.
func TestTwoKeysOnOneSecretStillRegisterOneCluster(t *testing.T) {
	r, c := newTestRegistrar(
		managedNS("tenant-e", "e"),
		secretWithKeys("tenant-e", "e-admin-kubeconfig", map[string]string{
			keyAdminConf: kamajiKubeconfig,
			keyAdminSvc:  kamajiSvcKubeconfig,
		}),
	)
	r.cfg.Providers = mustPresets(t, "kamaji")

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	list, err := c.CoreV1().Secrets(testTargetNS).List(context.Background(), metaV1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("two keys on one Secret produced %d registrations, want 1", len(list.Items))
	}
	// secretValue, not Data: the fake does not merge StringData the way a real
	// apiserver does, so a freshly CREATED Secret reads empty out of Data alone.
	if got := secretValue(&list.Items[0], "server"); got != "https://192.168.1.195:6443" {
		t.Errorf("server = %q, want admin.conf's address; the first key that parses wins", got)
	}
}

// A repeated key is a typo in a values file. Every present key is now its own
// candidate, so a duplicate would parse the same bytes twice and report the same
// failure twice while the operator learned nothing.
func TestDuplicateSecretKeysInOneProviderAreRejected(t *testing.T) {
	cfg := testConfig()
	cfg.Providers = []Provider{{
		Name:              "mytool",
		SecretNamePattern: "mytool-*",
		SecretKeys:        []string{keyAdminConf, keyAdminSvc, keyAdminConf},
	}}
	if err := cfg.Validate(); err == nil {
		t.Error("a duplicated secret key was accepted")
	}
}

// Every way a claim can be refused carries its own reason.
//
// The reason is the conflict metric's only label, so a site tagged with the wrong
// constant is a monitoring bug that no other test can see: the refusal still
// happens, the Secret is still protected, and only the counter lies. Deriving it
// from the message text instead would make a reworded sentence a silent
// regression, which is why it travels on the error.
func TestEveryRefusalCarriesItsOwnReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  string
		build func(t *testing.T) (*Registrar, child)
	}{
		{
			name: "a Secret we do not own at all",
			want: conflictNotManaged,
			build: func(*testing.T) (*Registrar, child) {
				hand := &coreV1.Secret{ObjectMeta: metaV1.ObjectMeta{
					Name:      "cluster-prod",
					Namespace: testTargetNS,
					Labels:    map[string]string{argoSecretTypeLabel: argoSecretTypeValue},
				}}
				r, _ := newTestRegistrar(hand)
				return r, child{cluster: "prod", namespace: "tenant-evil", server: testServer, config: "{}"}
			},
		},
		{
			name: "an orphan naming a different cluster",
			want: conflictOrphanClusterMismatch,
			build: func(*testing.T) (*Registrar, child) {
				existing := registeredSecret("a", testSourceNS)
				delete(existing.Labels, SourceNamespaceLabel(testPrefix))
				existing.Labels[ClusterLabel(testPrefix)] = "somethingelse"
				r, _ := newTestRegistrar(existing)
				return r, child{cluster: "a", namespace: testSourceNS, server: testServer, config: "{}"}
			},
		},
		{
			name: "a name another live namespace holds",
			want: conflictIncumbent,
			build: func(*testing.T) (*Registrar, child) {
				r, _ := newTestRegistrar(registeredSecret("a", "k3k-incumbent"))
				return r, child{cluster: "a", namespace: "k3k-challenger", server: testServer, config: "{}"}
			},
		},
		{
			name: "an unclaimed name an older namespace also wants",
			want: conflictContestedName,
			build: func(t *testing.T) (*Registrar, child) {
				// No Secret exists, deliberately: with one present this would take
				// the incumbency branch instead and pass under the wrong reason.
				r, c := newTestRegistrar(
					managedNSAt("k3k-older", "a", 2*time.Hour),
					managedNSAt("k3k-younger", "a", time.Hour),
				)
				if secretExists(t, c, "cluster-a") {
					t.Fatal("this case must reach the create path, not the incumbency check")
				}
				return r, child{cluster: "a", namespace: "k3k-younger", server: testServer, config: "{}"}
			},
		},
		{
			name: "two workers creating the same Secret at once",
			want: conflictCreateRace,
			build: func(*testing.T) (*Registrar, child) {
				r, c := newTestRegistrar(managedNS(testSourceNS, "a"))
				c.PrependReactor("create", resourceSecrets, func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apiErrors.NewAlreadyExists(
						schema.GroupResource{Resource: resourceSecrets}, "cluster-a")
				})
				return r, child{cluster: "a", namespace: testSourceNS, server: testServer, config: "{}"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c := tc.build(t)
			err := r.apply(context.Background(), c)
			var conflict *conflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("apply returned %v, want a conflictError", err)
			}
			if conflict.reason != tc.want {
				t.Errorf("reason = %q, want %q; the conflict metric would count this "+
					"refusal under the wrong label", conflict.reason, tc.want)
			}
		})
	}
}

// The binary and the chart must derive the same lease name.
//
// The chart computes it in _helpers.tpl. If they disagree, an instance deployed
// from a plain manifest and one deployed by Helm with identical labelPrefix and
// managedBy contend for the same cluster Secrets while holding different leases,
// and both reconcile: exactly what leader election is for. The literal below is
// what `helm template --set leaderElection.enabled=true` renders for the chart's
// default values, so changing either side alone fails here.
func TestLeaderElectionIDMatchesTheChart(t *testing.T) {
	const fromChart = "acr-991221237547448b"
	if got := LeaderElectionID(DefaultLabelPrefix, "cluster-registrar"); got != fromChart {
		t.Errorf("LeaderElectionID = %q, chart renders %q", got, fromChart)
	}
}

// Two installs that would contend for the same Secrets must contend for the same
// lease, and two that never meet must not.
func TestLeaderElectionIDSerialisesExactlyTheCollidingInstalls(t *testing.T) {
	base := LeaderElectionID(DefaultLabelPrefix, "cluster-registrar")
	if got := LeaderElectionID(DefaultLabelPrefix, "cluster-registrar"); got != base {
		t.Error("identical configuration produced different leases")
	}
	if got := LeaderElectionID(DefaultLabelPrefix, "other"); got == base {
		t.Error("a different managed-by shares a lease; installs that never meet are serialised")
	}
	if got := LeaderElectionID("example.com/", "cluster-registrar"); got == base {
		t.Error("a different label-prefix shares a lease")
	}
	if errs := validation.IsDNS1123Subdomain(base); len(errs) > 0 {
		t.Errorf("%q is not a valid object name: %s", base, strings.Join(errs, "; "))
	}
}

// ownedSecret is secretWith plus a controller ownerReference, i.e. what a
// provisioner writes rather than what a tenant can drop in beside it.
func ownedSecret(ns, name, key, body string) *coreV1.Secret {
	s := secretWith(ns, name, key, body)
	yes := true
	s.OwnerReferences = []metaV1.OwnerReference{{
		APIVersion: "k3k.io/v1alpha1", Kind: "Cluster", Name: "real", UID: "u1",
		Controller: &yes,
	}}
	return s
}

// Discovery matches on Secret name and key alone, both of which anyone with
// Secret write in the namespace chooses. `k3k-aaa-kubeconfig` sorts before
// `k3k-real-kubeconfig`, so on a pure name sort the planted one wins and ArgoCD
// is pointed at whatever server it names.
func TestPlantedSecretLosesToTheProvisionersOwn(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS(testNS, "a"),
		secretWith(testNS, "k3k-aaa-kubeconfig", "kubeconfig.yaml", tokenKubeconfig),
		ownedSecret(testNS, "k3k-real-kubeconfig", "kubeconfig.yaml", k3kKubeconfig),
	)

	ch, ok := discoverNS(t, r, testNS)
	if !ok {
		t.Fatal("expected one registration, got none")
	}
	if ch.server != k3kServer {
		t.Errorf("registered the planted secret: server = %q, want %q", ch.server, k3kServer)
	}
}

// The CAPI contract mandates the Secret type, so it is a provenance signal on
// the loosest pattern shipped -- which is the one most in need of one.
func TestCAPISecretTypeOutranksAnEarlierName(t *testing.T) {
	planted := secretWith("capi-c", "aaa-kubeconfig", "value", tokenKubeconfig)
	genuine := secretWith("capi-c", "c-kubeconfig", "value", capiKubeconfig)
	genuine.Type = "cluster.x-k8s.io/secret"

	r, _ := newTestRegistrar(managedNS("capi-c", "c"), planted, genuine)
	r.cfg.Providers = mustPresets(t, "capi")

	ch, ok := discoverNS(t, r, "capi-c")
	if !ok {
		t.Fatal("expected one registration, got none")
	}
	if ch.server != "https://192.168.1.196:6443" {
		t.Errorf("registered the planted secret: server = %q", ch.server)
	}
}

// Provenance orders candidates; it never excludes them. A fleet whose
// provisioner sets neither signal must keep registering exactly as before.
func TestSecretWithNoProvenanceStillRegisters(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS(testNS, "a"),
		secretWith(testNS, "k3k-a-kubeconfig", "kubeconfig.yaml", k3kKubeconfig),
	)

	if _, ok := discoverNS(t, r, testNS); !ok {
		t.Fatal("a secret with no ownerReference and no type must still register")
	}
}

// registeredAt is registeredSecret pinned to a specific server address, which is
// the identity ArgoCD actually keys a cluster by.
func registeredAt(cluster, srcNS, server string) *coreV1.Secret {
	s := registeredSecret(cluster, srcNS)
	s.Data["server"] = []byte(server)
	return s
}

// ArgoCD identifies a cluster by its address, and GetClusterByURL returns
// items[0], so two live registrations sharing a server are two claims on one
// identity and which one wins is an informer-index accident. Incumbency settles
// the cluster NAME; nothing settled the address.
func TestSecondNamespaceCannotClaimARegisteredServer(t *testing.T) {
	r, c := newTestRegistrar(
		// The incumbent's namespace must exist, or garbage collection removes its
		// registration before the collision is ever reached.
		managedNS("other-ns", "a"),
		registeredAt("a", "other-ns", k3kServer),
		managedNS(testNS, "b"),
		kubeconfigSecret(testNS, "k3k-b-kubeconfig"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Logf("reconcile reported: %v", err)
	}
	if secretExists(t, c, "cluster-b") {
		t.Error("registered a second cluster on an address ArgoCD already holds")
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("the incumbent registration was disturbed")
	}
}

// A demoted registration is invisible to ArgoCD, so it holds no identity and
// must not block the address being claimed again.
func TestDemotedRegistrationDoesNotHoldItsServer(t *testing.T) {
	demoted := registeredAt("a", "other-ns", k3kServer)
	delete(demoted.Labels, argoSecretTypeLabel)
	demoted.Labels[OrphanedSecretTypeLabel(testPrefix)] = argoSecretTypeValue

	r, c := newTestRegistrar(
		demoted,
		managedNS(testNS, "b"),
		kubeconfigSecret(testNS, "k3k-b-kubeconfig"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Logf("reconcile reported: %v", err)
	}
	if !secretExists(t, c, "cluster-b") {
		t.Error("a demoted registration blocked its address from being reclaimed")
	}
}

// The same namespace re-registering its own cluster is not a collision.
func TestOwnRegistrationIsNotAServerCollision(t *testing.T) {
	r, c := newTestRegistrar(
		registeredAt("a", testNS, k3kServer),
		managedNS(testNS, "a"),
		kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("a namespace was blocked from updating its own registration")
	}
}

// annotatedNS is managedNS with annotations on the source namespace.
func annotatedNS(name, cluster string, kv map[string]string) *coreV1.Namespace {
	ns := managedNS(name, cluster)
	ns.Annotations = kv
	return ns
}

// The ApplicationSet cluster generator reads {{metadata.annotations.<key>}} just
// as it reads labels, and an annotation has neither the 63-byte cap nor the
// charset a label value is held to. A URL cannot be a label value at all.
func TestPrefixedAnnotationsPropagate(t *testing.T) {
	r, c := newTestRegistrar(
		annotatedNS(testNS, "a", map[string]string{
			testPrefix + "grafana": "https://grafana.example.com/d/abc",
			"unprefixed":           "ignored",
		}),
		kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getSecret(t, c, "cluster-a")
	if got.Annotations[testPrefix+"grafana"] != "https://grafana.example.com/d/abc" {
		t.Errorf("prefixed annotation was not propagated: %v", got.Annotations)
	}
	if _, ok := got.Annotations["unprefixed"]; ok {
		t.Error("an unprefixed annotation was propagated")
	}
}

// The reserved suffixes are a trust boundary for annotations exactly as for
// labels, or a namespace could aim garbage collection at kube-system.
func TestReservedSuffixesAreNotPropagatedAsAnnotations(t *testing.T) {
	r, c := newTestRegistrar(
		annotatedNS(testNS, "a", map[string]string{
			SourceNamespaceLabel(testPrefix): "kube-system",
			PruneLabel(testPrefix):           PruneDisabled,
		}),
		kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getSecret(t, c, "cluster-a")
	for _, k := range []string{SourceNamespaceLabel(testPrefix), PruneLabel(testPrefix)} {
		if _, ok := got.Annotations[k]; ok {
			t.Errorf("reserved key %q was propagated as an annotation", k)
		}
	}
}

// Annotations allow 256KB and every registration sits in ArgoCD's cluster
// informer, so without a cap the source namespace picks ArgoCD's memory
// footprint. Dropped rather than truncated: a truncated URL looks like it works.
func TestOversizedAnnotationIsDroppedNotTruncated(t *testing.T) {
	big := strings.Repeat("x", annotationLimits.value+1)
	r, c := newTestRegistrar(
		annotatedNS(testNS, "a", map[string]string{
			testPrefix + "big":   big,
			testPrefix + "small": "kept",
		}),
		kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getSecret(t, c, "cluster-a")
	if _, ok := got.Annotations[testPrefix+"big"]; ok {
		t.Error("an oversized annotation was propagated")
	}
	if got.Annotations[testPrefix+"small"] != "kept" {
		t.Error("one oversized annotation dropped its well-behaved sibling")
	}
}

// Per-value was the only cap, so ~64 values still reached the apiserver's 256KB
// ceiling. apply() merges onto ArgoCD's own keys, so crossing it fails every
// later Update including the credential refresh: a tenant could wedge its own
// registration for good.
func TestAnnotationSetIsBoundedInTotalNotOnlyPerValue(t *testing.T) {
	ann := map[string]string{}
	// Each is under the per-value cap and together they are far over the total.
	// Sized so the TOTAL cap is what binds: at value-1 bytes each, the total runs
	// out after 8 keys and the count cap is never reached.
	for i := 0; i < annotationLimits.count+8; i++ {
		ann[fmt.Sprintf("%sk%02d", testPrefix, i)] = strings.Repeat("y", annotationLimits.value-1)
	}
	r, c := newTestRegistrar(
		annotatedNS(testNS, "a", ann),
		kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getSecret(t, c, "cluster-a")

	kept, bytes := 0, 0
	for k, v := range got.Annotations {
		if strings.HasPrefix(k, testPrefix) {
			kept++
			bytes += len(v)
		}
	}
	if kept > annotationLimits.count {
		t.Errorf("propagated %d annotations, over the %d cap", kept, annotationLimits.count)
	}
	if bytes > annotationLimits.total {
		t.Errorf("propagated %d bytes, over the %d cap", bytes, annotationLimits.total)
	}
	if kept == 0 {
		t.Error("the cap dropped everything rather than admitting what fits")
	}
}

// The count cap needs its own fixture. With values large enough to exhaust the
// byte budget the total cap always binds first, so a test built that way leaves
// the count limb uncovered and it could be deleted unnoticed.
func TestAnnotationSetIsBoundedInKeyCount(t *testing.T) {
	ann := map[string]string{}
	// Tiny values: count+8 of them is nowhere near the total byte budget.
	for i := 0; i < annotationLimits.count+8; i++ {
		ann[fmt.Sprintf("%sk%02d", testPrefix, i)] = "small"
	}
	if len(ann)*len("small") >= annotationLimits.total {
		t.Fatalf("fixture is too large to isolate the count cap: %d bytes", len(ann)*len("small"))
	}

	out, dropped := propagate(ann, testPrefix, annotationLimits)
	if len(out) != annotationLimits.count {
		t.Errorf("propagated %d annotations, want exactly the %d cap", len(out), annotationLimits.count)
	}
	if len(dropped) != 8 {
		t.Errorf("dropped %d annotations, want the 8 over the cap", len(dropped))
	}
	// Sorted admission means the survivors are the first N by key.
	for i := 0; i < annotationLimits.count; i++ {
		k := fmt.Sprintf("%sk%02d", testPrefix, i)
		if _, ok := out[k]; !ok {
			t.Errorf("%s was dropped although it sorts within the cap", k)
		}
	}
}

// Which annotations survive must not flap: map iteration order would rewrite the
// Secret every pass and hand ArgoCD a different set each time.
func TestTheAdmittedAnnotationSetIsStableAcrossPasses(t *testing.T) {
	ann := map[string]string{}
	for i := 0; i < annotationLimits.count+8; i++ {
		ann[fmt.Sprintf("%sk%02d", testPrefix, i)] = strings.Repeat("z", annotationLimits.value-1)
	}
	var first map[string]string
	for pass := 0; pass < 3; pass++ {
		out, _ := propagate(ann, testPrefix, annotationLimits)
		if pass == 0 {
			first = out
			continue
		}
		if !reflect.DeepEqual(out, first) {
			t.Fatalf("pass %d admitted a different set than pass 0", pass)
		}
	}
}

// 0.5.x stored the address as written, so normalising a trailing slash must not
// read as a move. It would run the collision check against a pair that has
// coexisted for months, and a refusal there leaves the untrimmed value in place
// to refuse again forever while the credential stops being refreshed.
func TestNormalisingATrailingSlashIsNotAnAddressMove(t *testing.T) {
	// A live incumbent already holds the same address. If the trailing slash reads
	// as a move, claimServer runs and refuses, and the Secret never gets rewritten.
	incumbent := registeredSecret("other", "other-ns")
	incumbent.Data["server"] = []byte(k3kServer)
	// It must exist, or claimServer releases the address as a dead incumbent and
	// the test would pass without exercising anything.
	otherNS := &coreV1.Namespace{ObjectMeta: metaV1.ObjectMeta{Name: "other-ns"}}

	// What 0.5.x stored: the address exactly as the kubeconfig wrote it.
	stored := registeredSecret("a", testNS)
	stored.Data["server"] = []byte(k3kServer + "/")

	r, c := newTestRegistrar(
		managedNS(testNS, "a"),
		kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
		stored, incumbent, otherNS,
	)

	before := testutil.ToFloat64(conflictsTotal.WithLabelValues(conflictServerCollision))
	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	after := testutil.ToFloat64(conflictsTotal.WithLabelValues(conflictServerCollision))

	if got := getSecret(t, c, "cluster-a"); string(got.Data["server"]) != k3kServer {
		t.Errorf("server = %q, want the normalised %q", got.Data["server"], k3kServer)
	}
	if after != before {
		t.Errorf("normalising a trailing slash counted %v server collisions", after-before)
	}
}

// The sweep must reach prefixed annotations, or a cluster could never be opted
// back out, and must not reach anything else.
func TestAnnotationSweepSparesForeignKeys(t *testing.T) {
	existing := registeredSecret("a", testNS)
	existing.Annotations = map[string]string{
		testPrefix + "gone":            "stale",
		"argocd.argoproj.io/something": "operator-set",
	}
	r, c := newTestRegistrar(
		managedNS(testNS, "a"),
		kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
		existing,
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getSecret(t, c, "cluster-a")
	if _, ok := got.Annotations[testPrefix+"gone"]; ok {
		t.Error("a prefixed annotation absent upstream survived the sweep")
	}
	if got.Annotations["argocd.argoproj.io/something"] != "operator-set" {
		t.Errorf("the sweep destroyed a foreign annotation: %v", got.Annotations)
	}
}

// The whole point of the capa-eks preset. Both Secrets satisfy `value`, and
// `<c>-kubeconfig` sorts before `<c>-user-kubeconfig`, so provider precedence is
// the only thing that can put the exec one first.
func TestCapaEksPresetPrefersExecOverTheExpiringToken(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS("eks", "c"),
		secretWith("eks", "c-kubeconfig", "value", tokenKubeconfig),
		secretWith("eks", "c-user-kubeconfig", "value", execKubeconfig),
	)
	r.cfg.Providers = mustPresets(t, "capa-eks", "capi")
	r.cfg.ExecCredentials = true

	ch, ok := discoverNS(t, r, "eks")
	if !ok {
		t.Fatal("expected one registration, got none")
	}
	if !strings.Contains(ch.config, "awsAuthConfig") {
		t.Errorf("registered the 15-minute token instead of the translated credential: %s", ch.config)
	}
}

// Declared the other way round, the expiring token wins. Documented behaviour,
// and the thing an operator will get wrong.
func TestProviderOrderDecidesWhichEksCredentialWins(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS("eks", "c"),
		secretWith("eks", "c-kubeconfig", "value", tokenKubeconfig),
		secretWith("eks", "c-user-kubeconfig", "value", execKubeconfig),
	)
	r.cfg.Providers = mustPresets(t, "capi", "capa-eks")
	r.cfg.ExecCredentials = true

	ch, ok := discoverNS(t, r, "eks")
	if !ok {
		t.Fatal("expected one registration, got none")
	}
	if !strings.Contains(ch.config, "bearerToken") {
		t.Errorf("expected the token to win on provider order: %s", ch.config)
	}
}

// The winner parses cleanly, so no parse error reports it and the registration
// looks healthy until the token runs out. Without this the only signal that a
// providers list is misordered is the absence of an Info line.
func TestPassingOverAnExecCandidateIsWarnedAbout(t *testing.T) {
	var buf bytes.Buffer
	r, _ := newTestRegistrar(
		managedNS("eks", "c"),
		secretWith("eks", "c-kubeconfig", "value", tokenKubeconfig),
		secretWith("eks", "c-user-kubeconfig", "value", execKubeconfig),
	)
	r.log = slog.New(slog.NewTextHandler(&buf, nil))
	r.cfg.Providers = mustPresets(t, "capi", "capa-eks")
	r.cfg.ExecCredentials = true

	if _, ok := discoverNS(t, r, "eks"); !ok {
		t.Fatal("expected one registration, got none")
	}
	if !strings.Contains(buf.String(), "cannot carry an exec credential") {
		t.Errorf("a misordered provider list registered silently: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "c-user-kubeconfig") {
		t.Error("the warning did not name the candidate it passed over")
	}
}

// The shape being exec-CAPABLE is not the same as the Secret carrying an exec
// block. A `<c>-user-kubeconfig` holding a plain certificate is nothing to fix,
// and warning about it every interval forever is how a real warning gets
// filtered out.
func TestNoWarningWhenThePassedOverSecretCarriesNoExec(t *testing.T) {
	var buf bytes.Buffer
	r, _ := newTestRegistrar(
		managedNS("eks", "c"),
		secretWith("eks", "c-kubeconfig", "value", tokenKubeconfig),
		secretWith("eks", "c-user-kubeconfig", "value", tokenKubeconfig),
	)
	r.log = slog.New(slog.NewTextHandler(&buf, nil))
	r.cfg.Providers = mustPresets(t, "capi", "capa-eks")
	r.cfg.ExecCredentials = true

	if _, ok := discoverNS(t, r, "eks"); !ok {
		t.Fatal("expected one registration, got none")
	}
	if strings.Contains(buf.String(), "cannot carry an exec credential") {
		t.Errorf("warned about a passed-over secret that has no exec block: %s", buf.String())
	}
}

// A kubeconfig carrying both a static credential and an exec block registers on
// the static one, so it can be the Secret that WINS while also being the one
// recorded as shadowed. Naming it on both sides of the warning reads as
// nonsense, so there should be no warning at all.
func TestNoWarningWhenTheShadowedSecretIsTheOneUsed(t *testing.T) {
	const staticAndExec = `apiVersion: v1
kind: Config
clusters:
- name: managed
  cluster:
    server: https://example.eks.amazonaws.com
    certificate-authority-data: Y2FkYXRh
users:
- name: managed
  user:
    client-certificate-data: Y2VydA==
    client-key-data: a2V5
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws-iam-authenticator
      args: [token, -i, my-eks-cluster]
`
	var buf bytes.Buffer
	r, _ := newTestRegistrar(
		managedNS("eks", "c"),
		secretWith("eks", "c-kubeconfig", "value", "not a kubeconfig at all"),
		secretWith("eks", "c-user-kubeconfig", "value", staticAndExec),
	)
	r.log = slog.New(slog.NewTextHandler(&buf, nil))
	r.cfg.Providers = mustPresets(t, "capi", "capa-eks")
	r.cfg.ExecCredentials = true

	ch, ok := discoverNS(t, r, "eks")
	if !ok {
		t.Fatal("expected the static credential to win after the first failed to parse")
	}
	// certData proves the winner was the static-credential Secret, i.e. the same
	// one that carries the exec block and so appears in `shadowed`.
	if !strings.Contains(ch.config, "certData") {
		t.Fatalf("expected the static credential to be carried: %s", ch.config)
	}
	if strings.Contains(buf.String(), "cannot carry an exec credential") {
		t.Errorf("warned while using the very secret it called passed over: %s", buf.String())
	}
}

// Ordered correctly there is nothing to say, and a warning on every namespace
// every interval is how a real one gets filtered out.
func TestCorrectProviderOrderWarnsAboutNothing(t *testing.T) {
	var buf bytes.Buffer
	r, _ := newTestRegistrar(
		managedNS("eks", "c"),
		secretWith("eks", "c-kubeconfig", "value", tokenKubeconfig),
		secretWith("eks", "c-user-kubeconfig", "value", execKubeconfig),
	)
	r.log = slog.New(slog.NewTextHandler(&buf, nil))
	r.cfg.Providers = mustPresets(t, "capa-eks", "capi")
	r.cfg.ExecCredentials = true

	if _, ok := discoverNS(t, r, "eks"); !ok {
		t.Fatal("expected one registration, got none")
	}
	if strings.Contains(buf.String(), "cannot carry an exec credential") {
		t.Errorf("a correctly ordered list still warned: %s", buf.String())
	}
}

// The global flag alone changes nothing: the provider must allow it too.
func TestGlobalFlagAloneDoesNotEnableTranslation(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS("capi-c", "c"),
		secretWith("capi-c", "c-kubeconfig", "value", execKubeconfig),
	)
	r.cfg.Providers = mustPresets(t, "capi")
	r.cfg.ExecCredentials = true

	if _, ok := discoverNS(t, r, "capi-c"); ok {
		t.Error("capi translated an exec credential; only exec-bearing shapes may")
	}
}

// And the provider alone changes nothing either.
func TestProviderAloneDoesNotEnableTranslation(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS("eks", "c"),
		secretWith("eks", "c-user-kubeconfig", "value", execKubeconfig),
	)
	r.cfg.Providers = mustPresets(t, "capa-eks")
	r.cfg.ExecCredentials = false

	if _, ok := discoverNS(t, r, "eks"); ok {
		t.Error("capa-eks translated with --exec-credentials off")
	}
}

func TestParseProviderExecSuffix(t *testing.T) {
	p, err := ParseProvider("mine=*-user-kubeconfig=value=exec")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !p.AllowExec {
		t.Error("the exec suffix did not reach AllowExec")
	}
	if _, err := ParseProvider("mine=*-kubeconfig=value=true"); err == nil {
		t.Error("a 4th field other than \"exec\" was accepted")
	}
	plain, err := ParseProvider("mine=*-kubeconfig=value")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if plain.AllowExec {
		t.Error("a 3-field spec enabled exec")
	}
}

// The chart flattens a providers[] entry into the --provider DSL and the binary
// parses it back. If the two drift, a custom provider silently stops being
// configurable. The lease name has the same pin from both sides, and it exists
// because those two did drift once.
//
// The literal is what `helm template --set providers[0].allowExec=true` renders.
func TestProviderSpecMatchesWhatTheChartRenders(t *testing.T) {
	const fromChart = "mytool=mytool-*-kubeconfig=admin.conf,admin.svc=exec"

	p, err := ParseProvider(fromChart)
	if err != nil {
		t.Fatalf("the binary cannot parse what the chart renders: %v", err)
	}
	if p.Name != "mytool" {
		t.Errorf("name = %q", p.Name)
	}
	if p.SecretNamePattern != "mytool-*-kubeconfig" {
		t.Errorf("pattern = %q", p.SecretNamePattern)
	}
	if !slices.Equal(p.SecretKeys, []string{keyAdminConf, keyAdminSvc}) {
		t.Errorf("keys = %v", p.SecretKeys)
	}
	if !p.AllowExec {
		t.Error("the chart's =exec suffix did not reach AllowExec")
	}
	// And the three-field form the chart renders without allowExec.
	plain, err := ParseProvider("mytool=mytool-*-kubeconfig=admin.conf,admin.svc")
	if err != nil {
		t.Fatalf("three-field form: %v", err)
	}
	if plain.AllowExec {
		t.Error("a chart entry without allowExec enabled exec")
	}
}

// capz-aks spells its glob the other way round from capa-eks: CAPZ appends
// "-user" to "<cluster>-kubeconfig" rather than inserting it. A wrong glob here
// matches nothing and the failure is silent.
func TestCapzAksMatchesCAPZsSpelling(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS("aks", "c"),
		secretWith("aks", "c-kubeconfig-user", "value", execKubeconfigWith(
			"kubelogin", "get-token", "--server-id", "6dae42f8-4368-4678-94ff-3960e28e3630")),
	)
	r.cfg.Providers = mustPresets(t, "capz-aks")
	r.cfg.ExecCredentials = true

	ch, ok := discoverNS(t, r, "aks")
	if !ok {
		t.Fatal("capz-aks did not match <cluster>-kubeconfig-user")
	}
	if !strings.Contains(ch.config, "execProviderConfig") {
		t.Errorf("no execProviderConfig emitted: %s", ch.config)
	}
	if !strings.Contains(ch.config, "AAD_SERVER_APPLICATION_ID") {
		t.Errorf("the AAD server application id was not carried: %s", ch.config)
	}
}

// CAPA's spelling must not match capz-aks, or one preset would swallow both and
// the distinction the README documents would be untrue.
func TestCapzAksDoesNotMatchTheCAPASpelling(t *testing.T) {
	r, _ := newTestRegistrar(
		managedNS("aks", "c"),
		secretWith("aks", "c-user-kubeconfig", "value", execKubeconfig),
	)
	r.cfg.Providers = mustPresets(t, "capz-aks")
	r.cfg.ExecCredentials = true

	if _, ok := discoverNS(t, r, "aks"); ok {
		t.Error("capz-aks matched CAPA's <cluster>-user-kubeconfig")
	}
}

// The collision check runs on UPDATE too, not only on create: an address that
// moves onto one another namespace already holds is the same collision, and it
// is the path a re-pointed cluster actually takes.
func TestServerCollisionIsCheckedWhenAnAddressMoves(t *testing.T) {
	// `a` already sits at the k3k fixture's address, from another namespace.
	incumbent := registeredAt("a", "other-ns", k3kServer)
	// `b` is registered from testNS at a different address, and its kubeconfig
	// now resolves to the address `a` holds.
	moving := registeredAt("b", testNS, "https://192.0.2.99")

	r, c := newTestRegistrar(
		managedNS("other-ns", "a"), incumbent,
		managedNS(testNS, "b"), kubeconfigSecret(testNS, "k3k-b-kubeconfig"), moving,
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Logf("reconcile reported: %v", err)
	}

	got := getSecret(t, c, "cluster-b")
	if string(got.Data["server"]) == k3kServer {
		t.Error("a registration moved onto an address another namespace already holds")
	}
}

// A failed LIST must not be read as "no collision". Called directly rather than
// through Reconcile: a reactor broad enough to reach this also kills the earlier
// LISTs, so the pass would prove nothing about this function.
func TestServerCollisionCheckFailsClosedOnAPIError(t *testing.T) {
	r, c := newTestRegistrar()
	c.PrependReactor("list", resourceSecrets, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apiErrors.NewInternalError(io.ErrUnexpectedEOF)
	})

	err := r.claimServer(context.Background(), child{
		cluster: "a", namespace: testNS, server: k3kServer,
	})
	if err == nil {
		t.Fatal("a failed collision check was treated as no collision")
	}
	// And it must not be a conflictError: nothing was proven, so the caller has
	// to retry rather than log a refusal and move on.
	var conflict *conflictError
	if errors.As(err, &conflict) {
		t.Errorf("an API failure was reported as a refusal: %v", err)
	}
}

// The audit is where an unreadable credential is actually noticed, and that
// branch was never exercised end to end.
func TestAuditBucketsAnUnreadableCredential(t *testing.T) {
	broken := registeredSecret("broken", testNS)
	broken.Data["config"] = []byte(`{"tlsClientConfig":{"certData":"!!!not base64"}}`)
	absent := registeredSecret("absent", testNS)
	delete(absent.Data, "config")

	r, _ := newTestRegistrar(broken, absent)
	if err := r.AuditUnrouted(context.Background()); err != nil {
		t.Fatalf("audit: %v", err)
	}

	if got := testutil.ToFloat64(registrations.WithLabelValues(stateActive, expiryUnreadable)); got != 1 {
		t.Errorf("credential_expiry=unreadable = %v, want 1", got)
	}
	if got := testutil.ToFloat64(registrations.WithLabelValues(stateActive, expiryAbsent)); got != 1 {
		t.Errorf("credential_expiry=absent = %v, want 1", got)
	}
}

// ArgoCD reads `project` and `shard` from the cluster Secret's Data, never from
// its labels, so a prefixed label of either name is inert to ArgoCD and useful
// to an ApplicationSet selector. Withholding it would break the selector and
// protect nothing, which is why neither is in reservedSuffixes.
func TestProjectAndShardLabelsStillPropagate(t *testing.T) {
	ns := managedNS(testNS, "a")
	ns.Labels[testPrefix+SuffixProject] = testProject
	ns.Labels[testPrefix+SuffixShard] = "2"

	r, c := newTestRegistrar(ns, kubeconfigSecret(testNS, "k3k-a-kubeconfig"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := getSecret(t, c, "cluster-a")
	if got.Labels[testPrefix+SuffixProject] != testProject {
		t.Errorf("the project label was withheld: %v", got.Labels)
	}
	if got.Labels[testPrefix+SuffixShard] != "2" {
		t.Errorf("the shard label was withheld: %v", got.Labels)
	}
	// And they must not reach ArgoCD's own keys, which live in Data.
	if _, ok := got.Data["project"]; ok {
		t.Error("a propagated label reached ArgoCD's project data key")
	}
	if _, ok := got.Data["shard"]; ok {
		t.Error("a propagated label reached ArgoCD's shard data key")
	}
}

// An existing registration carrying those labels must survive the upgrade. This
// is the regression the reservation would have caused: the sweep deleting a
// label an ApplicationSet selects on.
func TestExistingProjectLabelSurvivesReconcile(t *testing.T) {
	existing := registeredSecret("a", testNS)
	existing.Labels[testPrefix+SuffixProject] = testProject

	ns := managedNS(testNS, "a")
	ns.Labels[testPrefix+SuffixProject] = testProject

	r, c := newTestRegistrar(ns, kubeconfigSecret(testNS, "k3k-a-kubeconfig"), existing)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := getSecret(t, c, "cluster-a"); got.Labels[testPrefix+SuffixProject] != testProject {
		t.Errorf("an existing project label was swept: %v", got.Labels)
	}
}

// ArgoCD's cluster indexer keys on strings.TrimRight(server, "/") and
// GetClusterByURL trims the same way, so `https://x/` and `https://x` are one
// cluster to ArgoCD. An exact-byte comparison here would let a trailing slash
// walk past the collision check into the exact ambiguity it exists to prevent.
func TestTrailingSlashCannotEvadeTheServerCollisionCheck(t *testing.T) {
	kc := strings.Replace(k3kKubeconfig, k3kServer, k3kServer+"/", 1)

	r, c := newTestRegistrar(
		managedNS("other-ns", "a"),
		registeredAt("a", "other-ns", k3kServer),
		managedNS(testNS, "b"),
		secretWith(testNS, "k3k-b-kubeconfig", "kubeconfig.yaml", kc),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Logf("reconcile reported: %v", err)
	}
	if secretExists(t, c, "cluster-b") {
		t.Error("a trailing slash evaded the collision check")
	}
}

// And the stored address is normalised, so it matches what ArgoCD indexes.
func TestServerIsStoredNormalised(t *testing.T) {
	kc := strings.Replace(k3kKubeconfig, k3kServer, k3kServer+"/", 1)
	pk, err := parseKubeconfig([]byte(kc), false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pk.server != k3kServer {
		t.Errorf("server = %q, want %q", pk.server, k3kServer)
	}
}

// A cluster Secret this registrar does not own is ambiguous to ArgoCD just the
// same, so the check selects on ArgoCD's own label rather than ours.
func TestForeignClusterSecretAlsoHoldsItsAddress(t *testing.T) {
	foreign := &coreV1.Secret{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      "hand-written",
			Namespace: testTargetNS,
			Labels:    map[string]string{argoSecretTypeLabel: argoSecretTypeValue},
		},
		Data: map[string][]byte{"server": []byte(k3kServer)},
	}
	r, c := newTestRegistrar(
		foreign,
		managedNS(testNS, "b"),
		kubeconfigSecret(testNS, "k3k-b-kubeconfig"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Logf("reconcile reported: %v", err)
	}
	if secretExists(t, c, "cluster-b") {
		t.Error("registered onto an address a hand-written cluster Secret already holds")
	}
}

// An orphaned incumbent will be collected anyway; holding its address until then
// refuses a rebuild for no reason.
func TestDeadIncumbentReleasesItsAddress(t *testing.T) {
	r, c := newTestRegistrar(
		registeredAt("a", "vanished-ns", k3kServer),
		managedNS(testNS, "b"),
		kubeconfigSecret(testNS, "k3k-b-kubeconfig"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Logf("reconcile reported: %v", err)
	}
	if !secretExists(t, c, "cluster-b") {
		t.Error("a registration whose namespace is gone blocked the address")
	}
}

// Unless it is pinned. `prune: disabled` means the operator said keep it, and
// unpinning is how they say otherwise.
func TestPinnedDeadIncumbentKeepsItsAddress(t *testing.T) {
	pinned := registeredAt("a", "vanished-ns", k3kServer)
	pinned.Labels[PruneLabel(testPrefix)] = PruneDisabled

	r, c := newTestRegistrar(
		pinned,
		managedNS(testNS, "b"),
		kubeconfigSecret(testNS, "k3k-b-kubeconfig"),
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Logf("reconcile reported: %v", err)
	}
	if secretExists(t, c, "cluster-b") {
		t.Error("a pinned registration's address was reclaimed against the operator's intent")
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("the pinned registration was removed")
	}
}

// The label sweep exempts the prune opt-out; the annotation sweep must too, or
// pinning a registration via an annotation undoes itself one reconcile later.
func TestPruneAnnotationSurvivesTheAnnotationSweep(t *testing.T) {
	existing := registeredSecret("a", testNS)
	existing.Annotations = map[string]string{
		PruneLabel(testPrefix):  PruneDisabled,
		testPrefix + "leftover": "should-go",
	}
	r, c := newTestRegistrar(
		managedNS(testNS, "a"), kubeconfigSecret(testNS, "k3k-a-kubeconfig"), existing,
	)

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getSecret(t, c, "cluster-a")
	if got.Annotations[PruneLabel(testPrefix)] != PruneDisabled {
		t.Errorf("the prune opt-out was swept from annotations: %v", got.Annotations)
	}
	if _, ok := got.Annotations[testPrefix+"leftover"]; ok {
		t.Error("an ordinary prefixed annotation absent upstream survived the sweep")
	}
}
