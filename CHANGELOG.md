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
- Unit tests for kubeconfig parsing and label propagation.

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
- RBAC narrowed from all-verbs-on-all-resources across `core` and `apps`, to
  `namespaces` (get, list, watch) and `secrets` (get, list, watch, create,
  update, patch, delete).
- The chart no longer renders a Job or a Service.

### Fixed

- A Secret now has to match the name pattern *and* carry the configured key.
  Matching on name alone picked the wrong object whenever a provisioner writes
  more than one Secret under a shared prefix: vcluster's `vc-*` matches both
  `vc-<name>` (the kubeconfig) and `vc-config-<name>` (vcluster's own config),
  and the decoy sorts first, so the whole namespace was skipped.

### Removed

- The `loft-sh/vcluster` dependency and the vcluster CLI shell-out, taking 810
  lines out of `go.mod` and `go.sum`. Direct dependencies are now cobra,
  client-go and yaml.
- Flags `--clusters`, `--named-cluster` and `--auto-discovery`.
- Chart values `clusters` and `autoDiscovery`.

### Migrating from 0.1.x

1. `helm uninstall` the old release. The chart name changed, so this is not an
   upgrade.
2. Delete any cluster Secrets the old version created, or relabel them with
   `<prefix>managed-by=<value>` so the new version adopts and manages them.
   Without that label they are ignored, and will never be cleaned up.
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
