package registrar

import (
	"context"
	"testing"
	"time"

	coreV1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// demotedSecret is what a rename leaves behind: hidden from ArgoCD, kept, and
// stamped with when it happened.
//
// The stamp is passed in rather than taken from the clock, so the tests below
// need no injectable time source. `stale` is the age, not the timestamp.
func demotedSecret(cluster, srcNS string, stale time.Duration) *coreV1.Secret {
	s := registeredSecret(cluster, srcNS)
	s.UID = types.UID("uid-" + s.Name)
	s.ResourceVersion = "1000"
	delete(s.Labels, argoSecretTypeLabel)
	s.Labels[OrphanedSecretTypeLabel(testPrefix)] = argoSecretTypeValue
	s.Labels[SupersededByLabel(testPrefix)] = "cluster-new"
	s.Labels[StaleSinceLabel(testPrefix)] = time.Now().UTC().Add(-stale).Format(staleSinceFormat)
	return s
}

// renamedTo builds the state after a namespace has re-registered under a new
// name: the live registration, the demoted one, and the namespace itself.
func renamedTo(t *testing.T, newCluster string, old *coreV1.Secret, ttl time.Duration) (*Registrar, *fake.Clientset) {
	t.Helper()
	r, c := newTestRegistrar(
		managedNS(testNS, newCluster),
		kubeconfigSecret(testNS, "k3k-a-kubeconfig"),
		old,
	)
	r.cfg.DemotedTTL = ttl
	return r, c
}

// A demoted registration is deleted once it has been superseded for longer than
// the TTL.
func TestDemotedRegistrationIsDeletedOnceItsTTLExpires(t *testing.T) {
	r, c := renamedTo(t, "new", demotedSecret("old", testNS, 48*time.Hour), 24*time.Hour)

	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if secretExists(t, c, "cluster-old") {
		t.Error("a demoted registration outlived its TTL and was kept")
	}
	if !secretExists(t, c, "cluster-new") {
		t.Error("the live registration was collected along with the expired one")
	}
}

// ...and kept while it has not.
//
// Without this, deleting the TTL comparison outright still passes the test
// above: "always expire" and "expire when old" are indistinguishable from one
// direction.
func TestDemotedRegistrationIsKeptWhileItsTTLHasNotExpired(t *testing.T) {
	r, c := renamedTo(t, "new", demotedSecret("old", testNS, time.Hour), 24*time.Hour)

	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-old") {
		t.Error("a demoted registration was deleted an hour into a 24h TTL")
	}
}

// An unreadable stamp must never expire.
//
// time.Parse returns the ZERO time on failure. An ignored error therefore reads
// as the year 1, making every demoted registration about two thousand years old,
// and the first reconcile after upgrading would delete every one of them across
// the fleet. This is the single most expensive mistake available in this change.
func TestAnUnparseableStaleSinceStampNeverExpires(t *testing.T) {
	old := demotedSecret("old", testNS, 48*time.Hour)
	old.Labels[StaleSinceLabel(testPrefix)] = "not-a-timestamp"
	// One nanosecond: if the stamp were believed at all, this expires instantly.
	r, c := renamedTo(t, "new", old, time.Nanosecond)

	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-old") {
		t.Error("an unreadable stale-since stamp was treated as ancient and deleted")
	}
}

// A missing stamp must never expire either.
//
// Registrations demoted before the stamp existed carry no such label, and
// absence is not evidence of age. Same rule as
// TestRegistrationsWithoutARecordedUIDAreNeverCollectedAsStale.
//
// Two guards cover this, so removing either alone still passes: an empty stamp
// also fails to parse. That is defence in depth rather than a discriminating
// test, and it is worth knowing which this is.
func TestADemotedRegistrationWithoutAStaleSinceStampNeverExpires(t *testing.T) {
	old := demotedSecret("old", testNS, 48*time.Hour)
	delete(old.Labels, StaleSinceLabel(testPrefix))
	r, c := renamedTo(t, "new", old, time.Nanosecond)

	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-old") {
		t.Error("a registration with no stale-since stamp was expired")
	}
}

// Unset means never, which is what every existing installation gets on upgrade.
func TestTheDemotedTTLIsDisabledByDefault(t *testing.T) {
	if testConfig().DemotedTTL != 0 {
		t.Fatal("this test is meaningless unless the default is zero")
	}
	r, c := renamedTo(t, "new", demotedSecret("old", testNS, 10000*time.Hour), 0)

	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-old") {
		t.Error("a demoted registration was deleted with no TTL configured")
	}
}

// A terminating namespace must not expire anything.
//
// It reaches collection with applied == "", and a finalizer can still abort the
// deletion and bring the namespace back. Demotion is survivable there because it
// is reversible; a delete is not. This is what pins the TTL check BELOW that
// guard rather than above it.
func TestATerminatingNamespaceDoesNotExpireItsDemotedRegistrations(t *testing.T) {
	ns := managedNS(testNS, "new")
	now := metaV1.NewTime(time.Now())
	ns.DeletionTimestamp = &now
	ns.Finalizers = []string{"example.com/pending"}

	r, c := newTestRegistrar(ns, demotedSecret("old", testNS, 48*time.Hour))
	r.cfg.DemotedTTL = time.Nanosecond

	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-old") {
		t.Error("a demoted registration was expired while its namespace was still terminating")
	}
}

// The opt-out covers expiry too, which is now a third way a registration can be
// removed.
//
// Near-tautological as written, since the prune check sits above the demoted
// branch: the mutation it guards against is someone reordering the two.
func TestPruneDisabledSurvivesExpiry(t *testing.T) {
	old := demotedSecret("old", testNS, 48*time.Hour)
	old.Labels[PruneLabel(testPrefix)] = PruneDisabled
	r, c := renamedTo(t, "new", old, time.Nanosecond)

	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-old") {
		t.Error("an opted-out registration was expired")
	}
}

// The TTL delete must be preconditioned on the exact object that was read.
//
// UID alone is not enough here, unlike the orphan path: a reverted rename goes
// through apply's read-modify-write, which PRESERVES the UID. Only the
// resourceVersion moves, so only it can tell "still the demoted object I decided
// about" from "restored a moment ago".
//
// The fake ignores DeleteOptions entirely and never populates resourceVersion,
// so this needs a capture reactor and a fixture that sets one; without both it
// would compare "" to "" and pass with no precondition at all.
func TestTTLDeletionIsPreconditionedOnTheSecretsIdentity(t *testing.T) {
	old := demotedSecret("old", testNS, 48*time.Hour)
	if old.UID == "" || old.ResourceVersion == "" {
		t.Fatal("the fixture must carry both, or the assertions below are vacuous")
	}
	r, c := renamedTo(t, "new", old, 24*time.Hour)

	var gotUID types.UID
	var gotRV string
	var saw bool
	c.PrependReactor("delete", resourceSecrets, func(action k8stesting.Action) (bool, runtime.Object, error) {
		del, ok := action.(k8stesting.DeleteActionImpl)
		if !ok || del.GetName() != "cluster-old" {
			return false, nil, nil
		}
		saw = true
		if pre := del.DeleteOptions.Preconditions; pre != nil {
			if pre.UID != nil {
				gotUID = *pre.UID
			}
			if pre.ResourceVersion != nil {
				gotRV = *pre.ResourceVersion
			}
		}
		return false, nil, nil
	})

	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !saw {
		t.Fatal("the expired registration was never deleted")
	}
	if gotUID != old.UID {
		t.Errorf("delete precondition UID = %q, want %q", gotUID, old.UID)
	}
	if gotRV != old.ResourceVersion {
		t.Errorf("delete precondition resourceVersion = %q, want %q; a rename reverted "+
			"in the race window would be deleted anyway", gotRV, old.ResourceVersion)
	}
}

// Reverting the rename beats an expiring TTL.
//
// The live registration is checked before anything else in the loop, so the
// restored Secret is skipped as "the one we just applied" rather than considered
// for expiry. Moving the TTL check above that skip would delete the cluster that
// is currently registered.
func TestARevertedRenameBeatsAnExpiringTTL(t *testing.T) {
	// Same name the namespace is now registering under, and ancient.
	old := demotedSecret("a", testNS, 10000*time.Hour)
	r, c := renamedTo(t, "a", old, time.Nanosecond)

	if _, err := r.ReconcileOne(context.Background(), testNS); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !secretExists(t, c, "cluster-a") {
		t.Error("the registration this namespace is currently using was expired")
	}
}
