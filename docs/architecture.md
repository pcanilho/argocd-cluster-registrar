# Architecture

Notes for anyone changing this. Two decisions look like oversights and are not;
both have been tried the other way.

## Only namespaces are watched

Not the provisioner-written kubeconfig `Secret`s, even though watching them would
spot a credential rotation sooner.

k3k regenerates the child's keypair on *every one of its own reconciles*
(`ensureKubeconfigSecret` calls `generateKey()` unconditionally), so that `Secret`
changes far more often than the credential meaningfully does. The reconcile
interval is what keeps that from becoming one write per k3k reconcile against a
credential-bearing `Secret` in the ArgoCD namespace, each of which invalidates
ArgoCD's own cluster cache. In other words the interval is a coalescing rate
limiter, not a polling fallback.

Such a watch could not be narrowed either: the provisioner owns that `Secret`, so
it carries none of our labels. That is the same reason discovery is driven by the
namespace in the first place.

It would also hide a bug rather than expose one. A missing `RequeueAfter` would
be invisible on k3k, which writes constantly, while still breaking the three
quiet providers.

## Nothing is read through the controller's cache

Every read goes through `kubernetes.Interface`, direct.

The namespace existence proof in particular must not be cached. A label-filtered
cache reports an object that stops matching the selector as a synthetic deletion,
so a cached `NotFound` cannot tell a deleted namespace from one that merely lost
a label. Deregistering on the second would be catastrophic, and it is exactly
what `ReconcileOne` re-checks the ownership label to prevent.

The cache is configured with `ReaderFailOnMissingInformer`, so a cached read of
anything unconfigured is an error rather than a cluster-wide informer started
silently in the background. No `Secret` is ever held in it.

## Deletion requires positive proof

A registration is removed only on evidence that its source is gone, never on
absence from a desired set. There are three admissible proofs, and no others:

- the source namespace returns a definite `NotFound`;
- the namespace name is now held by a *different* object, proven by a recorded
  `source-namespace-uid` that does not match (`collectStaleUID`);
- a demoted registration has outlived `demotedTTL` (`deleteExpired`).

A transient API failure proves nothing, and every path that cannot evaluate a
namespace skips collection for it entirely rather than concluding anything.

Renaming is not a deletion: the old registration is *demoted*, which is
reversible, rather than deleted. See the README for what that means operationally.
