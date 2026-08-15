package registrar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// Pinning the registration a live namespace is actively using must stick.
//
// This is the documented case, and it was the one that did not work: apply's
// prefixed-label sweep removed the pin on the very next reconcile, and changed()
// reported the pin itself as drift, so the update that stripped it ran even when
// nothing else had moved. The operator set the label, the API accepted it, and it
// was gone within one interval. Every existing prune test pinned a Secret that
// apply never touches, so all five passed throughout.
func TestPinningAnActiveRegistrationSurvivesReconcile(t *testing.T) {
	existing := registeredSecret("a", testNS)
	existing.Labels[PruneLabel(testPrefix)] = PruneDisabled
	r, c := newTestRegistrar(
		managedNS(testNS, "a"),
		kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
		existing,
	)

	for i := range 3 {
		if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	got := getSecret(t, c, "cluster-a")
	if v := got.Labels[PruneLabel(testPrefix)]; v != PruneDisabled {
		t.Errorf("prune label = %q, want %q: the pin was swept off the registration "+
			"it was protecting", v, PruneDisabled)
	}
}

// The exemption is for the pin ALONE. The demotion labels must still be swept,
// because that sweep is what restores a registration when a rename is reverted.
func TestTheDemotionLabelsAreStillSweptOnRegistration(t *testing.T) {
	existing := registeredSecret("a", testNS)
	existing.Labels[OrphanedSecretTypeLabel(testPrefix)] = argoSecretTypeValue
	existing.Labels[SupersededByLabel(testPrefix)] = "cluster-a2"
	existing.Labels[StaleSinceLabel(testPrefix)] = "20260101T000000Z"
	r, c := newTestRegistrar(
		managedNS(testNS, "a"),
		kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
		existing,
	)

	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := getSecret(t, c, "cluster-a")
	for _, k := range []string{
		OrphanedSecretTypeLabel(testPrefix),
		SupersededByLabel(testPrefix),
		StaleSinceLabel(testPrefix),
	} {
		if v, ok := got.Labels[k]; ok {
			t.Errorf("%s = %q, want it swept; a reverted rename would never restore "+
				"the registration", k, v)
		}
	}
}

// A pinned registration must not be rewritten on every pass either.
//
// changed() reading the pin as drift meant a write per interval, forever, against
// a credential-bearing Secret, each one invalidating ArgoCD's cluster cache.
func TestAPinnedRegistrationIsNotRewrittenEveryPass(t *testing.T) {
	existing := registeredSecret("a", testNS)
	existing.Labels[PruneLabel(testPrefix)] = PruneDisabled
	want := registeredSecret("a", testNS)
	if changed(existing, want, testPrefix) {
		t.Error("the pin alone counts as drift, so a pinned registration is written " +
			"on every reconcile forever")
	}
}

// A delete that loses a UID precondition leaves the Secret in place deliberately,
// so the key must stay queued.
//
// Reporting it as finished retires the key, and the source namespace is gone, so
// nothing can ever enqueue it again: the registration outlives its cluster until
// someone restarts the process. That discards exactly the recovery the UID
// precondition was added to provide.
func TestAConflictedDeleteKeepsTheKeyQueued(t *testing.T) {
	r, c := newTestRegistrar(registeredSecret("gone", "k3k-gone"))
	c.PrependReactor("delete", resourceSecrets, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apiErrors.NewConflict(
			schema.GroupResource{Resource: resourceSecrets}, "cluster-gone", nil)
	})

	finished, err := r.ReconcileOne(context.Background(), "k3k-gone")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-gone") {
		t.Fatal("the fixture is wrong: the Secret should have survived the conflict")
	}
	if finished {
		t.Error("the key was retired while its registration is still there; its " +
			"namespace is gone, so nothing will ever visit it again")
	}
}

// A delete that genuinely removes the object still retires the key, or every
// namespace ever seen is revisited forever.
func TestASuccessfulDeleteStillRetiresTheKey(t *testing.T) {
	r, c := newTestRegistrar(registeredSecret("gone", "k3k-gone"))
	finished, err := r.ReconcileOne(context.Background(), "k3k-gone")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if secretExists(t, c, "cluster-gone") {
		t.Fatal("the orphan was not deleted")
	}
	if !finished {
		t.Error("a fully collected namespace must not stay on the requeue treadmill")
	}
}

// tls-server-name must reach ArgoCD.
//
// A kubeconfig reached by IP whose certificate is issued for a name depends on
// it. Dropping it yields a registration with the right CA and the right
// credentials that still fails hostname verification, which is the same class of
// unexplainable failure the file-path CA rejection exists to prevent.
func TestTLSServerNameIsCarriedThrough(t *testing.T) {
	const kc = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://10.0.0.5:6443
    certificate-authority-data: Y2FkYXRh
    tls-server-name: kubernetes.default.svc
users:
- name: u
  user:
    client-certificate-data: Y2VydGRhdGE=
    client-key-data: a2V5ZGF0YQ==
contexts:
- name: u@c
  context:
    cluster: c
    user: u
current-context: u@c
`
	pk, err := parseKubeconfig([]byte(kc), false)
	cfg := pk.config
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got argoClusterConfig
	if err := json.Unmarshal([]byte(cfg), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TLSClientConfig.ServerName != "kubernetes.default.svc" {
		t.Errorf("serverName = %q, want kubernetes.default.svc; the registration "+
			"would fail hostname verification", got.TLSClientConfig.ServerName)
	}
	if !strings.Contains(cfg, "serverName") {
		t.Error("serverName is absent from the marshalled config ArgoCD reads")
	}
}

// A kubeconfig without it must not gain an empty field, which ArgoCD would read
// as a server name of "".
func TestNoTLSServerNameEmitsNoServerName(t *testing.T) {
	pk, err := parseKubeconfig([]byte(k3kKubeconfig), false)
	cfg := pk.config
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Contains(cfg, "serverName") {
		t.Errorf("config carries an empty serverName: %s", cfg)
	}
}

// PresetNames feeds --help and the "no ArgoCD equivalent" error, so a preset
// added without a name here is invisible to a user reading either.
func TestPresetNamesListsEveryPresetSorted(t *testing.T) {
	got := PresetNames()
	if len(got) != len(presets) {
		t.Fatalf("PresetNames returned %d of %d presets: %v", len(got), len(presets), got)
	}
	for _, n := range got {
		if _, ok := presets[n]; !ok {
			t.Errorf("PresetNames returned %q, which is not a preset", n)
		}
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("PresetNames is unsorted, so help text order is nondeterministic: %v", got)
	}
}

// Both types carry a cause, and Unwrap is the only route to it: errors.As
// matches the wrapper itself without ever calling Unwrap, so nothing else in the
// package exercises these. Without this a broken Unwrap is invisible until a
// caller tries errors.Is against something the cause wraps.
func TestBothErrorTypesExposeTheirCause(t *testing.T) {
	cause := errors.New("the underlying failure")

	conflict := &conflictError{
		reason: conflictIncumbent,
		err:    fmt.Errorf("claim refused: %w", cause),
	}
	if !errors.Is(conflict, cause) {
		t.Error("conflictError hides its cause; errors.Is cannot reach it")
	}

	if !errors.Is(&apiFailure{fmt.Errorf("list secrets: %w", cause)}, cause) {
		t.Error("apiFailure hides its cause; errors.Is cannot reach it")
	}
}
