# Changelog

All notable changes to **argocd-cluster-registrar** are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
  rejects outright. Bumped to v2 and migrated `.golangci.yaml` accordingly.
- CI now runs `go test`, lints the chart with non-default values, and deploys it
  into kind to assert a cluster is registered and then garbage collected. The
  previous deploy test swallowed every assertion with `|| echo "Ignored..."`.
- Version stamping works. The `-X` ldflags targeted `cmd.version` rather than the
  full import path, so every released binary reported `dev (n/a) n/a`.
- `project_name` is pinned, since goreleaser derives it from the git remote and
  would otherwise name artefacts after the old repository.

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
