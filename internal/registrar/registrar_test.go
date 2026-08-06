package registrar

import (
	"context"
	"io"
	"log/slog"
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
	return Config{
		TargetNamespace:   "argocd",
		ManagedByValue:    testManagedBy,
		SecretNamePattern: "k3k-*-kubeconfig",
		SecretKey:         "kubeconfig.yaml",
		LabelPrefix:       testPrefix,
	}
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

	if err := r.collect(context.Background(), nil); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("cluster-a deleted while its source namespace still exists")
	}
}

func TestCollectDeletesWhenNamespaceIsActuallyGone(t *testing.T) {
	r, c := newTestRegistrar(registeredSecret("gone", "k3k-gone"))
	if err := r.collect(context.Background(), nil); err != nil {
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
	if err := r.collect(context.Background(), nil); err != nil {
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

	if err := r.collect(context.Background(), nil); err == nil {
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
	r.cfg.SecretNamePattern = "vc-*"
	r.cfg.SecretKey = "config"

	got, err := r.findKubeconfigSecret(context.Background(), "ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "vc-x" {
		t.Errorf("picked %q, want vc-x (the decoy sorts first)", got.Name)
	}
}

func TestConfigValidate(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"empty target namespace": func(c *Config) { c.TargetNamespace = "" },
		"empty managed-by":       func(c *Config) { c.ManagedByValue = "" },
		"empty secret key":       func(c *Config) { c.SecretKey = "" },
		"bad glob":               func(c *Config) { c.SecretNamePattern = "k3k-[" },
		"prefix without slash":   func(c *Config) { c.LabelPrefix = "example.com" },
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
	children, complete, err := r.discover(context.Background())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(children) != 0 {
		t.Errorf("expected the invalid name to be skipped, got %+v", children)
	}
	if complete {
		t.Error("a skipped namespace must mark the pass incomplete so GC is suppressed")
	}
}
