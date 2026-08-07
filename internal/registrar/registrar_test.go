package registrar

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

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
		Data: map[string][]byte{"name": []byte(cluster), "server": []byte("https://x"), "config": []byte("{}")},
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
		Namespace: testTargetNS,
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
		server: "https://x", config: "{}",
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
	if s := string(got.Data["server"]); s != "https://192.168.1.192" {
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
	c.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apiErrors.NewAlreadyExists(
			schema.GroupResource{Resource: "secrets"}, "cluster-a")
	})

	err := r.apply(context.Background(), child{
		cluster: "a", namespace: testSourceNS, namespaceUID: "uid-k3k-a",
		server: "https://x", config: "{}",
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
	c.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apiErrors.NewForbidden(
			schema.GroupResource{Resource: "secrets"}, "cluster-b", io.ErrUnexpectedEOF)
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
	if string(got.Data["server"]) != "https://192.168.1.192" {
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
	if err := r.collect(context.Background(), nil, nil); err != nil {
		t.Fatalf("collect: %v", err)
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

	c.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
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
