package registrar

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	coreV1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// conflictCounts snapshots every reason at once.
//
// DELTAS, never absolute values. The collectors are process-global and every
// test in this binary shares them, so an absolute assertion depends on which
// other tests ran first: TestEveryReturnPathRequeues alone produces a
// not_managed conflict. That is precisely how a test in this repo has passed by
// iteration-order luck before.
func conflictCounts(t *testing.T) map[string]float64 {
	t.Helper()
	out := make(map[string]float64, len(conflictReasons))
	for _, r := range conflictReasons {
		out[r] = testutil.ToFloat64(conflictsTotal.WithLabelValues(r))
	}
	return out
}

// Each refusal increments its own reason and nothing else.
//
// Asserting only that the total moved would pass just as happily if every
// refusal were labelled the same, which is the failure this is written against:
// the refusal is correct either way and only the counter lies. Requiring the
// other four to stay put is what makes the label mean something.
func TestEveryConflictReasonIsCountedUnderItsOwnLabel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  string
		ns    string
		build func(t *testing.T) *Registrar
	}{
		{
			name: "a Secret we do not own at all",
			want: conflictNotManaged,
			ns:   testNS,
			build: func(*testing.T) *Registrar {
				hand := &coreV1.Secret{ObjectMeta: metaV1.ObjectMeta{
					Name:      "cluster-a",
					Namespace: testTargetNS,
					Labels:    map[string]string{argoSecretTypeLabel: argoSecretTypeValue},
				}}
				r, _ := newTestRegistrar(
					managedNS(testNS, "a"), kubeconfigSecret(testNS, "k3k-a-kubeconfig"), hand)
				return r
			},
		},
		{
			name: "an orphan naming a different cluster",
			want: conflictOrphanClusterMismatch,
			ns:   testNS,
			build: func(*testing.T) *Registrar {
				existing := registeredSecret("a", testNS)
				delete(existing.Labels, SourceNamespaceLabel(testPrefix))
				existing.Labels[ClusterLabel(testPrefix)] = "somethingelse"
				r, _ := newTestRegistrar(
					managedNS(testNS, "a"), kubeconfigSecret(testNS, "k3k-a-kubeconfig"), existing)
				return r
			},
		},
		{
			name: "a name another live namespace holds",
			want: conflictIncumbent,
			ns:   testNS,
			build: func(*testing.T) *Registrar {
				r, _ := newTestRegistrar(
					managedNS(testNS, "a"), kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
					registeredSecret("a", "k3k-incumbent"))
				return r
			},
		},
		{
			name: "an unclaimed name an older namespace also wants",
			want: conflictContestedName,
			ns:   "k3k-younger",
			build: func(t *testing.T) *Registrar {
				r, c := newTestRegistrar(
					managedNSAt("k3k-older", "a", 2*testHour),
					kubeconfigSecret("k3k-older", "k3k-a-kubeconfig"),
					managedNSAt("k3k-younger", "a", testHour),
					kubeconfigSecret("k3k-younger", "k3k-a-kubeconfig"),
				)
				// With a registration already present this would refuse as an
				// incumbency instead, and pass under the wrong label.
				if secretExists(t, c, "cluster-a") {
					t.Fatal("this case must reach the create path")
				}
				return r
			},
		},
		{
			name: "two workers creating the same Secret at once",
			want: conflictCreateRace,
			ns:   testNS,
			build: func(*testing.T) *Registrar {
				r, c := newTestRegistrar(
					managedNS(testNS, "a"), kubeconfigSecret(testNS, "k3k-a-kubeconfig"))
				c.PrependReactor("create", resourceSecrets, func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apiErrors.NewAlreadyExists(
						schema.GroupResource{Resource: resourceSecrets}, "cluster-a")
				})
				return r
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.build(t)
			before := conflictCounts(t)

			if _, err := r.ReconcileOne(context.Background(), tc.ns); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			after := conflictCounts(t)
			for _, reason := range conflictReasons {
				delta := after[reason] - before[reason]
				want := float64(0)
				if reason == tc.want {
					want = 1
				}
				if delta != want {
					t.Errorf("conflicts_total{reason=%q} moved by %v, want %v",
						reason, delta, want)
				}
			}
		})
	}
}

// Adoption is a success, not a refusal, so it is counted somewhere else and a
// reader looking for it beside the conflict counter will not find it.
func TestAdoptingAnOrphanIsCountedSeparatelyFromRefusals(t *testing.T) {
	existing := registeredSecret("a", testNS)
	delete(existing.Labels, SourceNamespaceLabel(testPrefix))
	r, _ := newTestRegistrar(
		managedNS(testNS, "a"), kubeconfigSecret(testNS, "k3k-a-kubeconfig"), existing)

	beforeAdopt := testutil.ToFloat64(adoptionsTotal)
	beforeConflict := conflictCounts(t)

	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := testutil.ToFloat64(adoptionsTotal) - beforeAdopt; got != 1 {
		t.Errorf("adoptions_total moved by %v, want 1", got)
	}
	after := conflictCounts(t)
	for _, reason := range conflictReasons {
		if d := after[reason] - beforeConflict[reason]; d != 0 {
			t.Errorf("an adoption was also counted as a conflict{reason=%q}", reason)
		}
	}
}

// The audit pass reports how many registrations exist and how many are hidden
// from ArgoCD, which is the question the demotion TTL exists to answer.
//
// Gauges are asserted ABSOLUTELY, unlike the counters above: Set overwrites, so
// the value after the pass is entirely determined by this fixture. Asserting
// both states in one pass is what stops a leftover value from an earlier test
// satisfying either one on its own.
func TestDemotedRegistrationsAreCounted(t *testing.T) {
	demotedNames := []string{"old1", "old2", "old3"}
	objs := make([]runtime.Object, 0, len(demotedNames)+1)
	objs = append(objs, registeredSecret("live", testNS))
	for _, name := range demotedNames {
		demoted := registeredSecret(name, testNS)
		delete(demoted.Labels, argoSecretTypeLabel)
		demoted.Labels[OrphanedSecretTypeLabel(testPrefix)] = argoSecretTypeValue
		objs = append(objs, demoted)
	}
	r, _ := newTestRegistrar(objs...)

	if err := r.AuditUnrouted(context.Background()); err != nil {
		t.Fatalf("audit: %v", err)
	}

	if got := registrationsIn(t, stateDemoted); got != 3 {
		t.Errorf("registrations{state=demoted} = %v, want 3", got)
	}
	if got := registrationsIn(t, stateActive); got != 1 {
		t.Errorf("registrations{state=active} = %v, want 1", got)
	}
	// These fixtures carry no certData, so they must land in `none` rather than
	// be quietly counted as healthy.
	if got := testutil.ToFloat64(registrations.WithLabelValues(stateActive, expiryNone)); got != 1 {
		t.Errorf("registrations{state=active,credential_expiry=none} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(registrations.WithLabelValues(stateActive, expiryOK)); got != 0 {
		t.Errorf("a registration with no certificate was counted as ok")
	}
}

// registrationsIn sums a state across every expiry bucket, which is what the
// gauge meant before it gained the second dimension.
func registrationsIn(t *testing.T, state string) float64 {
	t.Helper()
	var total float64
	for _, bucket := range expiryBuckets {
		total += testutil.ToFloat64(registrations.WithLabelValues(state, bucket))
	}
	return total
}
