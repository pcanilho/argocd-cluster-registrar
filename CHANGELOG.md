# Changelog

All notable changes to **argocd-cluster-registrar** are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.6.1] - 2026-09-01

Maintenance only. No flag, metric, label, annotation or chart value moves, and
the reconcile and collection logic is untouched.

### Build

- **Go 1.27 is the new floor.** The `go` directive moves from 1.26.0 to 1.27.0,
  and the now-redundant `toolchain` line goes with it. This binds source builds
  only: `go install ...@v0.6.1` needs a 1.27 toolchain. The released binaries
  and the published image are unaffected.
- **`go fix` applied**, every rewrite semantics-preserving: `errors.As` against
  a declared target becomes `errors.AsType[T]`, a `strings.Split` consumed by a
  `range` becomes `strings.SplitSeq`, tests take `t.Context()`, and composite
  literals drop the `ObjectMeta` wrapper now that promoted fields are allowed in
  one. That last one accounts for most of the diff.
- **Kubernetes client libraries move to 0.37.0**, from 0.36.3.
- **The gosec action is pinned one commit past its own `v2.29.0` tag**, whose
  `action.yml` still names the 2.28.0 image: Go 1.26.5 with `GOTOOLCHAIN=local`,
  below the new floor. Upstream repinned it to the 2.29.0 image a day after
  tagging. Dependabot tracks tags, so v2.30.0 will move it forward again.

## [0.6.0] - 2026-08-17

### Security

- **Discovery no longer trusts a Secret on its name alone.** Candidates are now
  ordered by provenance -- the provisioner's `Secret.Type` and a controller
  `ownerReference` -- before name, so a Secret planted in a watched namespace
  loses to the one its provisioner wrote even when it sorts earlier. A candidate
  with neither signal is logged at Debug, not Warn: a provisioner that sets no
  `ownerReference` would otherwise log per namespace per interval forever. Run
  with `--debug` to audit it.

  **How much this buys depends on your provider.** Only `capi` declares a
  `Secret.Type`; for every other preset the sole available signal is the
  controller `ownerReference`. Neither is unforgeable by someone who can write
  Secrets in the watched namespace, though a dangling `ownerReference` at least
  gets the forgery garbage-collected. So this reorders candidates for any
  provider, but against a deliberate attacker it only raises the bar on `capi`.
  Constraining who may label a namespace remains the real control.
- **Two live registrations may no longer share a server URL.** ArgoCD identifies
  a cluster by its address and resolves a collision by informer-index order, so a
  second namespace could previously claim an address production already held.
  Refused now, on the same incumbency rule as the cluster name, and counted as
  `conflicts_total{reason="server_collision"}`.
- **`insecure-skip-tls-verify` is logged**, at Debug, for the same reason. It was
  copied through silently before, and it disables the CA pinning several other
  guarantees rest on.
- **`https://kubernetes.default.svc` is refused as a child's server.** ArgoCD
  special-cases that value to its own in-cluster config and ignores `caData`,
  `certData` and `keyData` entirely, so such a registration silently points at
  the management cluster and looks healthy.

### Added

- **Exec credentials are translated for managed control planes**, behind two
  independent opt-ins that are both off by default: a new `execCredentials` value
  and a per-provider allowance carried by the new `capa-eks` and `capz-aks`
  presets. `aws-iam-authenticator`, `aws eks get-token` and `kubelogin` become
  ArgoCD's `awsAuthConfig` or `execProviderConfig`; anything else is still
  refused; `heptio-authenticator-aws` is accepted as the legacy spelling of
  `aws-iam-authenticator`. The emitted command is always `argocd-k8s-auth`,
  never the source's,
  and only the target's identity is carried -- never the caller's. This removes
  the ~15-minute expiry that made CAPA's EKS registrations depend on `interval`.
- **Prefixed annotations propagate**, alongside labels and with the same reserved
  exclusions. The ApplicationSet cluster generator reads them as
  `{{metadata.annotations.*}}`, and unlike a label value they can hold a URL, a
  list, or anything over 63 bytes. Values over 4KiB are skipped and logged.
- **`registrations` gained a `credential_expiry` dimension**, read from the
  client certificate in the registration ArgoCD is holding: `expired`, `lt_24h`,
  `lt_7d`, `lt_30d`, `ok`, `token`, `exec`, `absent` or `unreadable`.
  Bearer-token clusters land in `token` rather than being counted as healthy;
  `exec` is a translated credential, which ArgoCD mints per connection and so has
  nothing to expire. The two are kept apart because they mean opposite things,
  and because `exec` is the only fleet-wide signal that translation actually ran.
  `absent` is kept distinct from `unreadable` because a registration with no
  config yet is not a damaged one.
- **`proxy-url` is carried** into ArgoCD's `config.proxyUrl` instead of being
  dropped in silence. `argocd cluster add` still ignores it. Refused when the
  URL embeds credentials, uses a scheme ArgoCD rejects, or accompanies
  `insecure-skip-tls-verify`.

- **The `ValidatingAdmissionPolicy` the README recommends now ships**, off by
  default under `admissionPolicy`. One policy constrains who may set
  `<prefix>managed-by`, an optional second requires `<prefix>cluster` to equal the
  namespace name. Kubernetes 1.30 or later, checked at render time: below that
  the kind is alpha or beta only, so an unguarded template would fail the whole
  release on "no matches for kind" and take the deployment with it. Note the
  check reads Helm's KubeVersion, which is a default when there is no cluster to
  ask and tracks the Helm release, so rendering offline under Helm 3.14 or older
  refuses the policy; use Helm 3.15+ or pass `--kube-version`. It
  ships as `Warn` and `Audit`, and enabling it with an empty allowlist fails the
  render rather than refusing every namespace in the cluster.

  Two things it does not do. Listing a controller in `authorizedUsers` grants
  that controller the label for every namespace it creates, so a self-service
  provisioner that passes caller-supplied labels through hands the label to
  everyone it serves -- put it behind a narrower path instead. And it constrains
  `<prefix>managed-by` only: the other prefixed labels, which are what an
  ApplicationSet actually selects on, are propagated as written.

### Changed

- A cluster's address changing is now logged at Warn. ArgoCD keeps watching the
  old one until its application controller restarts
  ([argo-cd#14410](https://github.com/argoproj/argo-cd/issues/14410)); the
  registration is still updated in place rather than replaced, because
  delete-and-recreate is not atomic and would discard ArgoCD's own keys.
- `--label-prefix` now refuses any prefix reaching the `argocd.argoproj.io/`
  domain, not just the one that could reach `secret-type`.
- **Propagated annotations are bounded as a set**, not only per value. 4KiB per
  value as before, plus 32KiB across all of them and 32 keys. The apiserver
  allows 256KB of annotations per object and a registration is written by
  merging onto ArgoCD's own keys, so an unbounded set let a source namespace push
  its own registration past that ceiling and fail every later write, the
  credential refresh included. Keys are admitted in sorted order, so which ones
  fit does not change between passes.
- **A misordered provider list is logged at Warn**, where the globs overlap.
  `*-kubeconfig` also matches `<cluster>-user-kubeconfig`, so declaring `capi`
  before `capa-eks` claims both of CAPA's Secrets and the exec-bearing one never
  becomes a candidate; the registration then looks healthy until its ~15-minute
  token runs out. Nothing reported this before, at any log level, because the
  losing candidate parsed cleanly. `capz-aks` uses `*-kubeconfig-user`, which
  `capi` does not match, so there is nothing to shadow and nothing to warn about
  there -- declaration order still decides which is preferred.
- Code comments lost 318 lines of release archaeology, prior-art citation and bug
  narration with no behaviour change. That material belongs in this file.

### Build

- **`sigstore/cosign-installer` is pinned to `@v4.1.2`, and the signature format
  changes with it.** The previous `@v3` floated to cosign v2.5.2, which is what
  v0.5.0 signed with. The v4 line installs cosign v3.0.6, where the protobuf
  bundle is the default format. **If you verify release signatures, check your
  command still works**; older `cosign verify` invocations may need updating.
  Pinned rather than floating so this never changes again by accident.
- **The image base is pinned** to `gcr.io/distroless/static:nonroot`, in both
  `.ko.yaml` (which CI reads) and `.goreleaser.yaml`'s `kos.base_image` (which the
  release reads; goreleaser does not read `.ko.yaml`). The previous default was
  `cgr.dev/chainguard/static`, whose free tier serves `latest` only, which is an
  unpinnable moving target in a build that otherwise sets `mod_timestamp`,
  `ko_data_creation_time` and `-trimpath`. **The published image's base layers
  change**; update any image-policy allowlist or scanner baseline keyed on the
  base registry.
- **Every CI action is on its latest release**: `actions/checkout` v7,
  `actions/setup-go` v7, `ko-build/setup-ko` v0.10, alongside the cosign bump
  above. `securego/gosec` stays SHA-pinned, already at v2.28.0.
- The kind fixtures carry a real certificate rather than a placeholder, so the
  expiry metric is exercised against something it can actually decode.

### Migrating from 0.5.x

`helm upgrade` is enough. No value changes meaning and no RBAC changes. What is
worth knowing before you do it:

1. **Audit any dashboard or alert that references `..._registrations`.** It gained
   a `credential_expiry` label, so a bare `registrations{state="active"}` now
   returns nine series instead of one, most of them zero. `sum(...)` and
   `sum by (state) (...)` are unaffected. A bare-selector threshold alert is not:
   a `>` alert now compares **per bucket** and may quietly stop firing, and a `<`
   alert starts firing against every empty bucket, which is a false page rather
   than a silent one. A single-value panel will show an arbitrary bucket. Wrap
   the selector in `sum()` and both directions are correct again.

2. **If you verify release signatures, re-check your command.** Signing moved
   from cosign v2 to v3, whose default bundle format differs. See *Build* above.

3. **A kubeconfig carrying `proxy-url` now routes ArgoCD through that proxy.**
   0.5.x dropped the field silently, so a registration that works today because
   ArgoCD reaches the cluster directly will start using the proxy. If the proxy is
   unreachable from the ArgoCD pod, that cluster's Applications go to `Unknown`.
   Remove `proxy-url` from the kubeconfig if it was vestigial.

4. **A kubeconfig with both a static credential and an `exec` block now
   registers**, using the static credential. 0.5.x refused it outright, so a
   namespace that was being skipped may gain a cluster in ArgoCD.

5. **`https://kubernetes.default.svc` is refused**, in every spelling including
   the `:443` and `.cluster.local` forms. Such a registration was always pointing
   ArgoCD at itself rather than at the child, because ArgoCD special-cases that
   address and ignores the CA and credentials next to it. The existing Secret is
   left alone but stops being refreshed; give the cluster a real address.

6. **No two cluster `Secret`s may share a `server` address.** ArgoCD identifies a
   cluster by its address, so duplicates were already resolved arbitrarily. The
   incumbent can be any ArgoCD cluster `Secret`, including a hand-written one or
   one from another registrar instance, not only a registration this tool owns.
   Refusals are logged at error and counted as
   `conflicts_total{reason="server_collision"}`. The check runs on creation and
   when an address moves, so existing pairs keep working until one is recreated.
   To scope one cluster several ways, use a single registration with ArgoCD's
   `namespaces` key, or `AppProject` destination service accounts.

   Worth knowing before you get one: a refusal on the *move* path returns before
   the write, so the `Secret` keeps its old address and the refusal repeats every
   interval while that cluster's credential stops being refreshed. It stays
   `active` in the gauge throughout. Alert on
   `conflicts_total{reason=~"incumbent|server_collision"}`; the log line names
   the cluster, the labels deliberately do not.

   Two addresses that reach the same cluster are still two registrations. Only a
   trailing slash is normalised, so `https://Host:6443`, `https://host:6443` and
   the IP form do not collide with each other.

7. **Where a namespace holds several matching `Secret`s, which one wins can
   change.** Candidates are now ordered by how much they look like their
   provisioner wrote them (a matching `Secret` type, a controller
   `ownerReference`) before falling back to name order. The `Secret`-type signal
   applies only to `capi`; for every other preset the ordering turns on whether a
   candidate carries a controller `ownerReference`. See *Security* above.

8. **The published image base changed.** See *Build* above.

9. **A server address ending in `/` is now stored without it**, matching how
   ArgoCD normalises it. Such a registration is rewritten in place on the first
   reconcile after the upgrade. Normalisation is compared on both sides, so it is
   not treated as an address change: no `cluster address changed` warning is
   logged and the collision check in note 6 is not run against a pair that has
   coexisted for months. The rewrite itself logs at Info like any other update.

10. **Prefixed *annotations* on a cluster `Secret` are now swept.** 0.5.x had no
    annotation handling at all, so anything hand-set under `--label-prefix` on a
    cluster `Secret` is removed on the first reconcile: the source namespace is
    the only source of truth for these. ArgoCD's own annotations and anything
    under another prefix are untouched.

11. **A kubeconfig this release starts refusing leaves its registration frozen,
    not removed.** That covers `https://kubernetes.default.svc` (note 5), a
    `proxy-url` carrying credentials or paired with `insecure-skip-tls-verify`,
    and an untranslatable `exec` block. The namespace is skipped, so the existing
    `Secret` keeps working on the credential it already holds and simply stops
    being refreshed. This is a `Warn` log only -- no counter moves and the gauge
    still reports it `active` -- so grep for `no usable kubeconfig` after
    upgrading.

## [0.5.0] - 2026-08-07

Closes the four things 0.4.0 deliberately left open: metrics, a TTL for demoted
registrations, key-level fallthrough on kubeconfig `Secret`s, and the absence of
any test driving a real manager.

Upgrading needs no configuration change, no RBAC change, and nothing new is
served or deleted unless you ask for it.

### Added

- **Prometheus metrics, off by default.** Four series, all counts of this
  instance's own decisions:

  | Metric | |
  |---|---|
  | `argocd_cluster_registrar_conflicts_total{reason}` | registrations refused, by which check refused them |
  | `argocd_cluster_registrar_adoptions_total` | orphaned `Secret`s adopted by a matching namespace |
  | `argocd_cluster_registrar_registrations{state}` | registrations owned, `active` or `demoted` |
  | `argocd_cluster_registrar_unrouted_secrets` | owned `Secret`s no reconcile key can reach |

  `conflicts_total{reason="incumbent"}` is the one worth alerting on: a contested
  cluster name persists until a human resolves it, and until now the only signal
  was a log line. `reason="create_race"` is benign and expected during a
  leader-election handover; do not alert on it.

  The counters are published at zero from startup, because an unincremented
  counter is absent from `/metrics` entirely and an absent series makes
  `increase(...[15m]) > 0` unevaluable rather than false.

  Enable with `metrics.enabled`, which opens the port on the pod;
  `metrics.service.enabled` additionally renders a `Service`. **The endpoint is
  unauthenticated**, so put a `NetworkPolicy` in front of it. Securing it properly
  means controller-runtime's authn/authz filter, which links `k8s.io/apiserver`
  and its cel-go/gRPC/OpenTelemetry arm and needs `tokenreviews` and
  `subjectaccessreviews` RBAC; nothing published carries a cluster name, a
  namespace or a credential, so that trade was not worth making. No
  `ServiceMonitor` ships: it needs a CRD `helm template` cannot check for, so it
  could not be proven correct in CI.

  **Which** cluster is contested stays in the log line, which names it along with
  both namespaces, and it is not coming to the metrics. Those values are read off
  objects a tenant creates, so as labels they are unbounded cardinality anyone
  able to label a namespace could mint, on the one code path that exists because
  somebody may be acting in bad faith. A per-conflict gauge would also be
  unclearable: when the losing claimant's namespace is deleted, the reconcile
  takes the `NotFound` branch and never reaches the code that would zero it, so it
  would alert forever about a conflict that no longer exists.

- **`demotedTTL`, off by default.** A renamed cluster leaves its old registration
  demoted rather than deleted, so the rename can be undone. Nothing ever cleaned
  those up: they accumulated until their source namespace was deleted, and they
  held the old cluster name against any other claimant for exactly as long.

  Setting `demotedTTL` deletes them once they have been superseded that long,
  which also frees the name they were holding, so a namespace refused for that
  name starts succeeding. It is opt-in because the TTL is equally a deadline on
  reverting a rename.

  Expiry is deliberately narrow. It runs only when the source namespace is alive
  and has registered under a different name, never while it is terminating or
  undiscoverable, and never sooner than `interval`. `<labelPrefix>prune: disabled`
  exempts a registration from it, as from the other two removal paths.

### Fixed

- **Every key present on a kubeconfig `Secret` is now tried, in declared order.**
  Discovery stopped at the first key a `Secret` carried, so each provider/`Secret`
  pair produced exactly one candidate. Kamaji ships `admin.conf` and `admin.svc`
  together, so a half-written or otherwise unusable `admin.conf` put `admin.svc`
  out of reach and left the namespace unregistered for as long as it stayed that
  way, with a working kubeconfig sitting beside it. `Provider.SecretKeys`
  documented its keys as "tried in order" throughout.

  Fallthrough across `Secret`s already worked and is unchanged; this extends the
  same mechanism within one. Scope worth being plain about: both Kamaji keys
  normally parse, so `admin.conf` still wins on a healthy install. This makes the
  declared order real rather than changing which key anyone uses today.

- **A repeated key in a provider spec is now rejected at startup** instead of
  parsing the same bytes twice, matching how a duplicate provider name is already
  handled.

- **`<labelPrefix>prune: disabled` now works on an active registration.** It was
  swept off by the same cleanup that removes a label deleted upstream, because it
  has no upstream to be absent from. `changed()` also read the pin as drift, so the
  update that stripped it ran even when nothing else had moved: the operator set
  the label, the API accepted it, and it was gone within one `interval`. That is
  the documented case, and it silently did not work. Present since the opt-out
  shipped in 0.4.0.

  Every existing test pinned a `Secret` that `apply` never touches, which is why
  five of them passed throughout. The demotion labels are still swept, since that
  is what restores a registration when a rename is reverted.

- **A registration whose deletion loses its UID precondition is no longer
  abandoned.** The delete leaves the `Secret` in place on purpose, but it was
  counted as removed, which retired the reconcile key. The source namespace is
  gone by then, so nothing could ever enqueue it again and the registration
  outlived its cluster until the process restarted. That discarded exactly the
  recovery the precondition was added for.

- **`tls-server-name` is carried into the cluster `Secret`.** It was parsed by
  nothing and dropped, so a kubeconfig reached by IP with a certificate issued for
  a name produced a registration with the right CA and the right credentials that
  still failed hostname verification.

- **`--leader-election-id` now defaults to the same lease the chart derives.** The
  chart computes it from `labelPrefix` and `managedBy`, because those are what
  decide whether two instances contend for the same cluster `Secret`s; the binary
  defaulted to a constant. So an instance deployed from a plain manifest and a
  chart-deployed one with identical configuration held *different* leases and both
  reconciled, which is the state leader election exists to prevent, reached by not
  using the chart. Set the flag explicitly to override. Only affects installs with
  `leaderElection` enabled, which is off by default.

- **An SPDX SBOM ships alongside each binary archive.** The images already had
  one, generated natively by ko, so only the archives were uncovered. Verified by
  a local snapshot build: eight documents, 57 packages each.

- **Errors are printed once.** `main` wrapped every failure in a second message on
  a second stream in a different format, while cobra had already printed it, and
  the wording was inherited from `vcluster-argocd-exporter` so a flag validation
  error read as a failure to export clusters.

- **An empty `--label-prefix` is rejected rather than silently repaired.** With no
  prefix, label propagation matched every label on the source namespace and the
  reserved set computed as bare names matching nothing actually written, so a
  tenant could propagate `argocd.argoproj.io/secret-type` off its own namespace.
  The constructor defaulted it, so this was unreachable through the CLI or the
  chart, but validation and defaulting disagreeing is how it stops being
  unreachable.

- **A `bindAddress` that is not `:PORT` now fails the chart render, naming the
  key.** The container port is derived by stripping the colon, so `0.0.0.0:8081`
  rendered `containerPort: 0` and a bare `8081` handed the binary an address it
  could not listen on. Both failed, neither said why.

- **A `--label-prefix` that ArgoCD's own key falls under is rejected.** Under
  `argocd.argoproj.io/`, a source namespace could propagate
  `argocd.argoproj.io/secret-type`, which is not a reserved suffix, and the
  propagated labels are copied last, so it would override the key this tool sets.

### Changed

- The `--dry-run` de-escalation of `--leader-elect` no longer writes back into the
  flag variable. No user-visible effect; it made the flag mapping untestable and
  leaked state across cases in the test binary.

- The README no longer restates the chart's values. Nine keys were documented in
  two places while twenty-three existed, so `leaderElection`, `probes`,
  `replicaCount` and `resources` read as though they were not configurable. Use
  `helm show values`. The architecture notes moved to
  [docs/architecture.md](docs/architecture.md), where they can grow without
  standing between a reader and the install instructions.

### Internal

- A test now drives a real manager under envtest. The startup seeder had no
  coverage anywhere: removing it leaves every other test passing while a
  registration orphaned during downtime survives forever with no symptom. Two kind
  steps cover the same ground against real RBAC.

- CI asserts `k8s.io/apiserver` stays out of the binary. One import of
  controller-runtime's metrics filter brings the whole arm back, and that cost is
  the entire reason metrics were deferred a release.

### Migrating from 0.4.x

Nothing to change. No RBAC change, no value changes meaning, and the two new
features are both off until you turn them on.

1. **Metrics are off.** `metrics.enabled` opens an unauthenticated port on the
   pod; pair it with a `NetworkPolicy`. `metrics.service.enabled` is a separate
   switch for the `Service`.

2. **`demotedTTL` is `0s`, meaning never.** Existing demoted registrations are
   untouched until you set it. When you do, remember it is also how long a
   mistaken rename stays revertible.

3. **Discovery may now find a cluster it previously could not.** If a namespace
   was stuck unregistered because the first key on its `Secret` was unusable, it
   will register on first reconcile after upgrade. That is the fix working.

4. **A duplicated `secretKeys` entry now fails at startup** rather than being
   quietly tolerated. It has never done anything useful; remove the duplicate.

## [0.4.0] - 2026-08-07

A controller. Registration and removal now follow namespace events instead of a
sixty-second poll, so they happen about as fast as the API server delivers one.

Also closes a privilege escalation present in 0.2.0 and 0.3.0, and stops a
renamed cluster leaving a working duplicate behind. Both are described below.

Upgrading needs no configuration change. It does need new RBAC, which `helm
upgrade` applies for you; see "Migrating from 0.3.x".

### Added

- **Runs as a controller.** Namespace events drive registration and removal, so a
  new cluster appears in ArgoCD in about as long as the API server takes to
  deliver one, and a deleted one disappears just as fast. Built on
  controller-runtime.

  Source kubeconfig `Secret`s are deliberately **not** watched. k3k regenerates
  the child's keypair on every one of its own reconciles, so that `Secret`
  changes far more often than the credential meaningfully does; watching it would
  turn each of those into a write against a credential-bearing `Secret` in the
  ArgoCD namespace. Such a watch could not be narrowed either, since the
  provisioner owns that `Secret` and it carries none of our labels. The reasoning
  is in the README's Architecture section.

- **`/healthz` and `/readyz`**, on `:8081` by default, tunable under `probes`.
  There were no probes at all before, so a wedged process was invisible.

- **Optional leader election**, off by default. Enabling it needs `leases` and
  `events` in `targetNamespace`. The lease is named for `labelPrefix` and
  `managedBy` rather than the release, because those decide whether two installs
  collide at all. `replicaCount` above one now requires it.

- **`<labelPrefix>prune: disabled`** on a cluster `Secret` opts that one
  registration out of both deletion and demotion. Removal is event-driven now and
  therefore near-instant, where a mistake used to have up to a full interval to be
  noticed.

- **`--once` is a supported way to run this without installing it.** It performs a
  single sweep, never builds a manager, never takes a lease, and falls back to
  your own kubeconfig. With `--dry-run` it prints every decision it would make,
  including refusals, without writing anything.

### Fixed

- **A cluster `Secret` is never taken over by a namespace that did not create it.**
  `apply` did an unconditional `Get`, and if `cluster-<name>` existed it was
  overwritten and relabelled as ours no matter who wrote it. Anyone able to label a
  namespace `<prefix>managed-by` and `<prefix>cluster` could therefore repoint an
  existing registration at their own API server with their own credentials and,
  because the takeover rewrote `source-namespace` too, make it garbage collectable
  on their terms. Present in 0.2.0 and 0.3.0. Reaching it needs the ability to set
  labels on a namespace, which the documented deployment model does not grant to
  tenants; the new "Who is allowed to set these labels" section of the README says
  what to do if yours does.

  An owned `Secret` that records no source namespace is still adoptable, but only
  by the cluster it already names.

- **Renaming a cluster no longer leaves a working duplicate behind.** Changing a
  namespace's `<prefix>cluster` label registered the new name and stranded the old
  `Secret` forever, because its source namespace still existed. That was not inert:
  `apply` only runs over what discovery returned, so the stale `Secret` was never
  rewritten and went on working from a frozen kubeconfig for as long as its
  certificate lasted. With two registrations sharing one server URL, which one
  ArgoCD acts on is undefined.

  The old registration is now **demoted** rather than deleted: its
  `argocd.argoproj.io/secret-type` label is parked under
  `<prefix>orphaned-secret-type`, and `<prefix>superseded-by` and
  `<prefix>stale-since` are added. ArgoCD stops seeing it at once, but nothing is
  destroyed, so the `namespaces`, `clusterResources` and `project` keys ArgoCD
  writes, plus any annotations you added, all survive. Reverting the rename
  restores the registration, credentials and all. Demoted `Secret`s are still
  collected once their namespace is gone.

  A demoted registration keeps its cluster name reserved, which is what makes the
  revert possible: another namespace claiming that name is refused while it exists.
  Delete it if the name should pass to someone else.

### Changed

- **Two namespaces claiming one cluster name no longer skip both.** Whoever holds
  the registration keeps it and the other namespace is refused and logged; an
  unclaimed name contested by several namespaces goes to the **oldest** claimant.
  Previously neither registered, so a stale or copy-pasted namespace could deny a
  healthy cluster its registration indefinitely. A refusal is logged at error level
  but does not fail the pass, because a contested name persists until a human
  fixes it.

  A namespace that never produced a usable kubeconfig also used to poison a healthy
  namespace claiming the same name, because the name was claimed before the
  kubeconfig was read. It no longer does.

- Garbage collection selects owned `Secret`s on `<prefix>managed-by` alone rather
  than also requiring ArgoCD's `secret-type` label, so demoted registrations stay
  collectable. Only `Secret`s carrying the ownership label are ever eligible, as
  before.

- **A namespace deleted and recreated under the same name no longer strands its
  predecessor's registration.** Namespace names are reusable and UIDs are not, so a
  registration recording a UID other than the one the namespace now carries has a
  source that is provably gone. Previously, if the replacement never became
  discoverable, every path returned early rather than conclude anything about it
  and the old registration went on pointing at a destroyed API server. Only
  registrations that actually record a UID are eligible, so nothing written before
  this release is affected.

- Cluster `Secret`s now record `<prefix>source-namespace-uid`, and the garbage
  collection delete is preconditioned on the `Secret`'s own UID. Under a watch the
  gap between proving a namespace gone and removing its registration is event
  latency rather than microseconds, so without this a delete decided in one
  reconcile could land on a `Secret` a later one had already recreated.

- **`interval` means something different.** It is now the requeue period: how long
  before a settled cluster is looked at again, and therefore the bound on how
  stale a credential can be after a certificate rotation. It is no longer the
  latency for registering or deregistering anything. The default is unchanged and
  no action is needed.

- Two instances sharing a `managedBy` value no longer merely "fight". Neither
  will take over a registration the other holds; if they are configured with
  different `providers` they rewrite each other's provider label, and
  `leaderElection` is the fix.

- `capi` promoted from *assumed* to *tested*. Verified against Cluster API v1.13.4
  with the Docker infrastructure provider: the kubeadm control-plane controller
  wrote `capi-child-kubeconfig` in the `Cluster`'s namespace, typed
  `cluster.x-k8s.io/secret`, labelled `cluster.x-k8s.io/cluster-name`, with the
  kubeconfig under `value` and no other key. Registering it and rebuilding a
  kubeconfig from the resulting ArgoCD Secret authenticated to the workload cluster
  as `kubernetes-admin` with full x509 verification. CAPD is what was exercised,
  but the Secret is written by the control-plane provider, so the shape holds for
  any infrastructure provider.
- README trimmed. The per-provider sections had grown to 43% of the page, most of
  it test provenance rather than instruction. Provenance lives here now; the README
  keeps the table, the configuration forms and the operational gotchas.

### Migrating from 0.3.x

Nothing to change. `helm upgrade` is enough for a default install, and no value
changes meaning in a way that requires action.

What is worth knowing before you upgrade:

1. **The behaviour changes from this release apply on first start.** If you have a
   cluster name claimed by two namespaces, or a registration that was previously
   "fixed" by relabelling a namespace, you will see refusals logged at error
   level. That is the takeover fix working. See *Fixed* above; the resolution is
   to remove the duplicate claim.

2. **RBAC widens.** `watch` on `namespaces`, cluster-wide. Nothing new in
   `targetNamespace` unless you enable
   `leaderElection`, which adds `leases` (get/create/update) and `events`
   (create/patch) there. `helm upgrade` applies all of it, but if you mirror this
   chart's RBAC into your own GitOps repo, or gate writes to the ArgoCD namespace
   with an admission policy, allow the new rules first.

   Note `watch` is **not** granted on `secrets` anywhere, and cluster-wide access
   to them is still `list` only: they are read directly and never cached.

3. **Probes are new.** If a `NetworkPolicy` restricts ingress to the registrar
   pod, permit the kubelet to reach `:8081` or the pod will fail its probes after
   upgrade.

4. **Deletion is immediate.** Removing a namespace, or mistyping its `cluster`
   label, now takes effect within seconds rather than up to a minute. Use
   `<labelPrefix>prune: disabled` on any registration you want pinned.

5. **`leaderElection` is available and off.** Turn it on only if you can grant
   `leases` and `events` in `targetNamespace`.

Changing `managedBy` or `labelPrefix` still orphans every existing registration,
and now also changes which lease you contend for. See "Changing `managedBy` or
`labelPrefix` later" in the README.

## [0.3.0] - 2026-08-07

Serves more than one provisioner at a time. Until now `Config` held a single
secret-name pattern and a single key, so an instance could register k3k **or**
vcluster and never both. The obvious workaround, a second Deployment, is a
trap: two instances sharing a `managedBy` value garbage collect each other's
Secrets.

Adds Kamaji and the Cluster API contract alongside the existing two, and fixes
two pre-existing defects found while making room for them.

Backwards compatible: a `0.2.x` values file keeps working unchanged. `providers`
ships **empty** so that a values file which sets only the deprecated
`secretNamePattern`/`secretKey` still takes the legacy path. Setting both forms is
now a chart-render failure rather than a silent preference for one.

### Added

- `providers`, a list of provisioner shapes tried in precedence order, replacing
  the single `secretNamePattern`/`secretKey` pair. Presets ship for `k3k`,
  `vcluster`, `kamaji` and `capi`; a custom shape can be given in full as
  `{name, secretNamePattern, secretKeys}`.
- **Kamaji**: `*-admin-kubeconfig`, keys `admin.conf` or `admin.svc`. Two keys,
  because Kamaji switches to `admin.svc` when its control plane advertises a
  service address. Verified against Kamaji v1.0.0: a real `TenantControlPlane`
  registered, and the resulting credentials authenticated to the tenant API server
  with full x509 verification. The same `Secret` also carries `super-admin.conf`,
  which is deliberately not preferred.
- **Cluster API**: `*-kubeconfig`, key `value`. A mandatory control-plane contract
  rather than a convention, so the one entry covers any CAPI cluster whatever the
  infrastructure provider, plus standalone k0smotron, which adopts the same shape.
  Scoped to self-managed control planes: CAPA/CAPZ/CAPG hand out exec credentials,
  which cannot become an ArgoCD Secret at all, or ~15-minute tokens, which can and
  then quietly expire.
- `<labelPrefix>provider` on each cluster Secret, recording which provider
  matched, so an ApplicationSet can select by provisioner. This makes the first
  pass after upgrade rewrite every existing registration: the label is new,
  nothing else about them changes.
- Candidate fallthrough. A Secret matching a provider's shape but failing to parse
  no longer poisons the namespace; the next candidate is tried. That is what makes
  CAPA's `<cluster>-user-kubeconfig` decoy harmless, rather than leaving the
  outcome to depend on `k` sorting before `u`.
- Validation for duplicate provider names, and a test for the empty-pattern case
  the code has always checked but nothing exercised.

### Fixed

- **Garbage collection is no longer suppressed fleet-wide by one bad namespace.**
  Any managed namespace without a usable kubeconfig cleared a single `complete`
  flag, which skipped `collect()` entirely, so one permanently broken namespace
  kept every other cluster's dead registration in ArgoCD indefinitely. Every k3k
  child hits this for its first ~90 seconds, during which GC was off for the whole
  fleet. Unevaluable namespaces are now exempted individually, and everything else
  is collected normally.

  One consequence worth knowing: if a cluster **moves namespaces**, the old
  namespace is provably gone while the new one is still unresolved, so the
  registration is now deleted and re-created a pass or two later. Applications
  targeting it can go briefly Unknown. Under the old fleet-wide flag there was no
  gap, because nothing was collected at all.
- **Reserved labels can no longer be spoofed from a source namespace.** Propagated
  labels are copied over the ones the tool computes, and `source-namespace` was
  not withheld, so a namespace labelling itself
  `<prefix>source-namespace: kube-system` produced a registration whose GC proof
  pointed at a namespace that never disappears, making it permanently
  uncollectable. All reserved suffixes are now withheld.

### Changed

- `--secret-name-pattern` and `--secret-key` are deprecated in favour of
  `--provider`, and kept as a single-provider shorthand. They cannot be combined
  with `--provider`; the binary rejects that rather than silently preferring one.
  The chart renders one form or the other, never both. Passing both is
  indistinguishable from meaning both, which is how a stale values file ends up
  quietly ignoring `providers`.
- The tagline, chart description, image description and `--help` text no longer
  enumerate k3k and vcluster, which stopped being accurate at four provisioners.
  The chart `keywords` still name them, deliberately, since that is what people
  search for. The provisioner table carries the specifics and keeps an honest
  `Status` column: `tested` means run against the real thing, `assumed` means
  taken from upstream source but not exercised here.
- README's RBAC section corrected. It claimed Secret writes were cluster-scoped;
  they have been a namespaced Role bound to `targetNamespace` since 0.2.0.

## [0.2.0] - 2026-08-06

Renames the project from `vcluster-argocd-exporter` and rewrites it as a
general cluster registrar. It no longer shells out to the vcluster CLI, no
longer runs as a one-shot Job, and now deletes registrations for clusters that
have gone away.

Everything about this release is breaking. The module path, binary, image and
chart name all change, so the ghcr chart and image start fresh at `0.2.0`
rather than continuing the old packages. There is no in-place upgrade from
`0.1.x`: uninstall the old release, then install this one. See **Migrating from
0.1.x** below.

Verified against k3k v1.2.0-rc3 and vcluster 0.36.1 on a Kubernetes 1.26 host.

### Added

- Garbage collection. ArgoCD cluster Secrets whose source namespace no longer
  exists are deleted. Only Secrets carrying the ownership label are eligible,
  so clusters registered by hand are never touched. `0.1.x` created Secrets but
  never removed them, leaving a dead entry in ArgoCD for every destroyed
  cluster.
- Continuous reconciliation, on `--interval` (default `60s`). Each pass
  re-reads every kubeconfig, which keeps a registration valid after a k3s
  server restart rotates the child's client certificate. Previously that left
  ArgoCD failing authentication with no visible cause.
- Client certificate support, alongside the existing bearer tokens. k3k and
  vcluster 0.36 both issue certificates where older vcluster issued a token.
- `--label-prefix`, defaulting to `argocd-cluster-registrar/`. The prefix used
  to be a hardcoded personal domain, which made the tool unusable as shipped by
  anyone else.
- `--dry-run`, which logs intended changes without writing. Useful when first
  pointing the registrar at an existing cluster, to confirm the GC selector
  matches only what you expect.
- Tests for the paths that can lose data: garbage collection against a fake API
  server (including a transient list failure, a still-present namespace, and
  hand-registered Secrets), field preservation on update, decoy-Secret
  selection, multi-context kubeconfig resolution, and config validation.

### Changed

- Deployment instead of Job. A Job only runs at sync time, and a cluster is
  normally destroyed long after the sync that created it, so a Job can register
  a cluster but can never deregister one.
- Discovery is namespace-driven. Namespaces labelled
  `<prefix>managed-by=<value>` are inspected for a Secret matching
  `--secret-name-pattern`. Matching on the namespace rather than the Secret is
  deliberate: the provisioner creates that Secret (k3k gives it an
  `ownerReference` to the `Cluster`), so it carries none of our labels and
  there is nowhere to add them.
- Labels under the configured prefix are copied from the source namespace onto
  the cluster Secret, so an ApplicationSet cluster generator can select on them.
- RBAC narrowed from all-verbs-on-all-resources across `core` and `apps`, and
  split by scope. Reads stay cluster-wide because discovery is label-driven
  (`namespaces` get/list, `secrets` list), but every write is now a namespaced
  Role bound to `targetNamespace` alone. Granting Secret create/update/delete
  across the whole cluster was a privilege-escalation path in exchange for
  nothing, since the tool only ever writes to one namespace. `watch` and `patch`
  are gone; neither was ever called.
- The chart no longer renders a Job or a Service.
- `image.tag` defaults to the chart's `appVersion`. The release workflow
  repackages the chart from the git tag without touching `values.yaml`, so a
  pinned tag there quietly shipped the previous image with a new chart.
- Release notes come from `CHANGELOG.md` rather than commit subjects, and a tag
  with no matching section fails the release.
- Go toolchain bumped to 1.26.5 (`go.mod`), and `go fix` applied: two manual
  map-copy loops are now `maps.Copy`.

### Fixed

- Garbage collection no longer deletes registrations it merely failed to see.
  Any error reading a managed namespace, an unparseable kubeconfig, or a missing
  cluster label used to drop that cluster from the desired set, and absence was
  treated as deletion. One transient API error could deregister the entire fleet
  while logging it as "no kubeconfig secret yet; will retry". Deletion now
  requires the source namespace to return NotFound, and a pass that could not
  evaluate every namespace skips collection entirely.
- Registrations no longer clobber fields this tool does not own. Updates were a
  whole-object replace, which silently dropped ArgoCD's own `namespaces`,
  `clusterResources` and `project` keys along with any operator-set annotations.
  It is now a read-modify-write.
- Kubeconfigs with several contexts are resolved through `current-context`
  instead of taking the first cluster and the first user. Those are not
  necessarily a matching pair, and mismatching them made ArgoCD present one
  cluster's credential to another cluster.
- Kubeconfigs that cannot work are rejected with an accurate message rather than
  registered: empty or unparseable server URLs, credentials referenced by file
  path, and `exec`/`auth-provider` plugins.
- Invalid and duplicate cluster names are skipped instead of failing forever. A
  name that cannot form a DNS-1123 Secret name was retried every pass, and two
  namespaces claiming one name flip-flopped a single Secret between them.
- A single undeletable Secret no longer stalls garbage collection for every
  other cluster.
- API calls now carry a 30 second timeout. Without one a hung request wedged the
  reconcile loop permanently while the pod stayed Ready.
- Invalid flags are rejected at startup rather than misbehaving quietly.
  `--interval 0` panicked, and an empty `--target-namespace` made the tool
  operate across every namespace.
- A Secret now has to match the name pattern *and* carry the configured key.
  Matching on name alone picked the wrong object whenever a provisioner writes
  more than one Secret under a shared prefix: vcluster's `vc-*` matches both
  `vc-<name>` (the kubeconfig) and `vc-config-<name>` (vcluster's own config).
  Whichever sorted first won, so the namespace was often skipped entirely.

### Build

- CI could not lint: `golangci-lint-action` v9 was fed a v1 version, which it
  rejects outright. Bumped to v2.12 and migrated `.golangci.yaml` accordingly.
  Note the CI and local versions must match: 2.11 and 2.12 disagree about
  `goconst`, so a clean local run said nothing while CI failed.
- CI now runs `go test`, lints the chart with non-default values, and deploys it
  into kind to assert a cluster is registered and then garbage collected. The
  previous deploy test swallowed every assertion with `|| echo "Ignored..."`.
- Version stamping works. The `-X` ldflags targeted `cmd.version` rather than the
  full import path, so every released binary reported `dev (n/a) n/a`.
- `project_name` is pinned, since goreleaser derives it from the git remote and
  would otherwise name artefacts after the old repository.
- The published chart and image are now linked to this repository on GHCR, via
  `org.opencontainers.image.source`. Helm copies Chart.yaml annotations onto the
  OCI manifest, and GHCR only makes the association when that source is an https
  URL. The chart had no annotations at all and the image used goreleaser's
  `{{ .GitURL }}`, which resolves to the SSH remote, so every 0.1.x release was
  published as an orphan package under the account.
- Chart metadata filled in: a real description, home, sources, icon, keywords,
  maintainers and a license annotation, none of which existed.

### Removed

- The `loft-sh/vcluster` dependency and the vcluster CLI shell-out, taking 810
  lines out of `go.mod` and `go.sum`. Direct dependencies are now cobra,
  client-go and yaml.
- Flags `--clusters`, `--named-cluster` and `--auto-discover`.
- Chart values `clusters` and `autoDiscovery`.

### Migrating from 0.1.x

1. `helm uninstall` the old release. The chart name changed, so this is not an
   upgrade.
2. Delete the cluster Secrets the old version created. They are named
   `vcluster-<name>`, whereas this version writes `cluster-<name>`, so both
   would otherwise sit in ArgoCD pointing at the same server. Do not try to
   adopt them by relabelling: the name is what identifies a registration, so a
   relabelled `vcluster-foo` is treated as an orphan and deleted on the next
   pass rather than reused.
3. Label each namespace holding a kubeconfig Secret with
   `<prefix>managed-by` and `<prefix>cluster`. The old version took a list of
   cluster names on the command line; that list no longer exists.
4. Install the new chart. Adjust `secretNamePattern` and `secretKey` if you are
   not on k3k; the defaults target it.

## [0.1.x]

Released as `vcluster-argocd-exporter`. Individual releases up to 0.1.8 are not
itemised here; see the
[release history](https://github.com/pcanilho/argocd-cluster-registrar/releases)
for details.
