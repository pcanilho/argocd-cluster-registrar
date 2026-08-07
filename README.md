[![RELEASE](https://github.com/pcanilho/argocd-cluster-registrar/actions/workflows/release.yaml/badge.svg)](https://github.com/pcanilho/argocd-cluster-registrar/actions/workflows/release.yaml)
[![TEST](https://github.com/pcanilho/argocd-cluster-registrar/actions/workflows/test.yaml/badge.svg)](https://github.com/pcanilho/argocd-cluster-registrar/actions/workflows/test.yaml)
[![Dependabot Updates](https://github.com/pcanilho/argocd-cluster-registrar/actions/workflows/dependabot/dependabot-updates/badge.svg)](https://github.com/pcanilho/argocd-cluster-registrar/actions/workflows/dependabot/dependabot-updates)
[![SAST](https://github.com/pcanilho/argocd-cluster-registrar/actions/workflows/sast.yaml/badge.svg)](https://github.com/pcanilho/argocd-cluster-registrar/actions/workflows/sast.yaml)

[![Version](https://img.shields.io/github/v/release/pcanilho/argocd-cluster-registrar?label=version&sort=semver)](https://github.com/pcanilho/argocd-cluster-registrar/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/pcanilho/argocd-cluster-registrar)](go.mod)
[![License](https://img.shields.io/github/license/pcanilho/argocd-cluster-registrar)](LICENSE)
<p align="center" width="100%">
    <img src="https://github.com/pcanilho/argocd-cluster-registrar/blob/main/docs/images/logo.png?raw=true" width="220"></img>
    <br>
    <i><b>argocd-cluster-registrar</b></i>
    <br>
    A cluster registrar made for ArgoCD
    <br>
    <br>
    ⚙️ <a href="#installing">Installing</a> | 🔎 <a href="#configuring">Configuring</a> | 🧩 <a href="#how-it-works">How it works</a> | 🔐 <a href="#operating">Operating</a>
    <br>
    <br>
</p>

If you are running clusters inside your cluster with
[k3k](https://github.com/rancher/k3k), [vcluster](https://www.vcluster.com/),
[Kamaji](https://kamaji.clastix.io/) or [Cluster API](https://cluster-api.sigs.k8s.io/),
and you use [ArgoCD](https://argoproj.github.io/argo-cd/), stop reading and jump to
[installing](#installing)!

Why? You have probably noticed that ArgoCD does not detect them out-of-the-box. The
provisioner writes a kubeconfig `Secret` into the cluster's namespace, but ArgoCD only
reads `Secret`s in its own namespace labelled
`argocd.argoproj.io/secret-type: cluster`, so you end up registering clusters by hand,
or with a somewhat painful ad-hoc script.

I have got you covered. This does it for you, and it also does the part scripts
usually skip:

* Registers each child cluster it finds, so ArgoCD can target it by name.
* Deletes the registration when the cluster is gone. Otherwise a destroyed
  cluster leaves a dead entry in ArgoCD forever.
* Re-reads every kubeconfig on a timer, on top of reacting to changes. A k3s
  server restart rotates the child's client certificate, and without this ArgoCD
  quietly starts failing authentication.

> [!IMPORTANT]
> Upgrading from `0.3.x`? Nothing to change, but registration is event-driven now
> and the RBAC widens. See
> [Migrating from 0.3.x](CHANGELOG.md#migrating-from-03x).
>
> Upgrading from `0.1.x` (`vcluster-argocd-exporter`)? The flags, values, Secret
> names and labels all changed, and a stale values file fails silently. See
> [Migrating from 0.1.x](CHANGELOG.md#migrating-from-01x). That section predates
> `providers`; where it tells you to set `secretNamePattern`/`secretKey`, prefer a
> `providers` entry instead.

## Installing

Install it once, as a singleton. Do not add it as a dependency of a per-cluster
chart: every instance reconciles cluster-wide, so a second one is at best doing
the same work twice. Give each instance its own `managedBy` if you do run more
than one. Two instances sharing that value but configured with different
`providers` will rewrite each other's `<labelPrefix>provider` label on every
reconcile, forever. Setting `leaderElection.enabled` makes that safe instead:
whichever acquires the lease runs, and the other waits.

### Helm `dependency`

```yaml
dependencies:
  - name: argocd-cluster-registrar
    version: ">=0.2.0"
    repository: oci://ghcr.io/pcanilho/charts
```

### Helm `standalone`

```bash
helm upgrade <release_name> --install \
  oci://ghcr.io/pcanilho/charts/argocd-cluster-registrar \
  -n <namespace> --create-namespace
```

## Configuring

### Values

```yaml
# Namespace ArgoCD reads cluster Secrets from. ArgoCD only looks in its own
# namespace, so in practice this is always "argocd".
targetNamespace: argocd

# Prefix for the labels read off the source namespace and copied onto the cluster
# Secret. Change it to match an existing labelling convention.
labelPrefix: argocd-cluster-registrar/

# Value of the `<labelPrefix>managed-by` label. It picks which namespaces to
# watch, and which cluster Secrets this release owns. Give each instance its own.
managedBy: cluster-registrar

# Provisioners to look for, in precedence order. Presets: k3k, vcluster, kamaji,
# capi. Empty means the binary's own default, which is k3k. See "Providers" below.
providers: []

# How long before a settled cluster is looked at again. NOT how quickly a new one
# is registered: that follows the namespace event. This bounds credential
# freshness after a certificate rotation.
interval: 60s

# Log what would change without writing anything. Useful the first time you point
# this at an existing cluster, to check the GC selector matches only what you expect.
dryRun: false

# Verbose logging.
debug: false
```

The binary also falls back to your own kubeconfig when it is not running in a
cluster, so you can try it before installing anything:

```bash
argocd-cluster-registrar --once --dry-run --debug
```

### Providers

This registers clusters provisioned *inside* the host cluster: something running
here writes a kubeconfig `Secret` into a namespace you can label. A standalone
cluster elsewhere has no such object, so there is nothing to discover.

`providers` lists the provisioner shapes to look for, **in precedence order**.
Each preset is a `Secret`-name glob plus the keys that may hold the kubeconfig.
`Status` is meant literally: **tested** means run against the version shown.

| Preset | Provisioner | `Secret` name | Key(s) | Status |
|---|---|---|---|---|
| `k3k` | [k3k](https://github.com/rancher/k3k) v1.2.0-rc3 | `k3k-*-kubeconfig` | `kubeconfig.yaml` | tested |
| `vcluster` | [vcluster](https://www.vcluster.com/) 0.36.1 | `vc-*` | `config` | tested, see below |
| `kamaji` | [Kamaji](https://kamaji.clastix.io/) v1.0.0 standalone | `*-admin-kubeconfig` | `admin.conf`, `admin.svc` | tested, see below |
| `capi` | [Cluster API](https://cluster-api.sigs.k8s.io/) v1.13.4 contract | `*-kubeconfig` | `value` | tested, see below |

Nothing currently ships as **assumed**, but the column stays so it can stay
honest if that changes.

Several can run at once, which is rather the point. One instance serves a mixed
fleet:

```yaml
providers:
  - k3k
  - capi
```

Anything else that writes a kubeconfig into a `Secret` works too, spelled out in
full:

```yaml
providers:
  - name: mytool
    secretNamePattern: "mytool-*-kubeconfig"
    secretKeys: [kubeconfig]
```

The matched provider is recorded on the cluster `Secret` as
`<labelPrefix>provider`, so an ApplicationSet can select by provisioner.

#### Per-provider notes

**Order matters.** The globs overlap on purpose: `capi`'s `*-kubeconfig` also
matches k3k's `k3k-<cluster>-kubeconfig`. Correctness comes from the key, not the
name, and where two providers could both claim a `Secret` the one declared first
wins. Put the more specific provider first. `capi` is the loosest shipped.

**Kamaji** normally writes both `admin.conf` and `admin.svc`. Only the first key
present is tried, so `admin.conf` wins; reorder them in a custom entry if you need
the other. Running Kamaji through its Cluster API control-plane provider produces
a second, CAPI-shaped `Secret` for the same cluster, so with both presets enabled
the one declared first decides which is used.

**`capi`** is the mandatory control-plane contract, so it covers any CAPI cluster
whatever the infrastructure provider, plus standalone k0smotron. It does **not**
usefully cover managed cloud control planes: CAPA's EKS path writes an `exec`
credential, which cannot become an ArgoCD `Secret` at all, and the CAPI-internal
one holds a token that rotates every ~15 minutes. Treat it as self-managed only.

**vcluster** exports a kubeconfig pointing at `https://localhost:8443`, which is
fine for a port-forward and useless to ArgoCD. This is the most common reason a
vcluster registration silently does not work. Point it somewhere ArgoCD can reach:

```yaml
controlPlane:
  service:
    spec:
      type: LoadBalancer
exportKubeConfig:
  server: https://<address>
```

If connections then fail x509 verification, the address is not on the API server
certificate; add it:

```yaml
controlPlane:
  proxy:
    extraSANs:
      - <address>
```

### Marking a cluster for registration

Both labels below are required. A namespace carrying `managed-by` but no
`cluster` is skipped with a warning, and the cluster name must be usable as a
Kubernetes object name, since the resulting Secret is called `cluster-<name>`.

Cluster names are unique across the fleet, and a collision resolves by
**incumbency**: whoever holds a registration keeps it, and another namespace
claiming that name is refused and logged. If nobody holds it yet, the **oldest**
claiming namespace wins. A registration is never taken over, so a cluster you
registered by hand, or one belonging to a different registrar, is left alone.

Label the namespace that holds the kubeconfig `Secret`. It reads the namespace
rather than the `Secret` because the provisioner owns that `Secret`. k3k, for
example, gives it an `ownerReference` to the `Cluster`, so it carries none of
your labels and there is nowhere to put them.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: k3k-sandbox
  labels:
    argocd-cluster-registrar/managed-by: cluster-registrar   # ownership
    argocd-cluster-registrar/cluster: sandbox                # the ArgoCD cluster name
    argocd-cluster-registrar/flux: "true"                    # copied to the Secret
```

Any label under the same prefix is copied onto the cluster `Secret`, except the
handful this tool writes itself (`managed-by`, `cluster`, `source-namespace`,
`provider`, and the demotion and prune markers below). Copying is how an
ApplicationSet cluster generator selects on them:

```yaml
generators:
  - clusters:
      selector:
        matchLabels:
          argocd-cluster-registrar/flux: "true"
```

## How it works

```mermaid
flowchart LR
    subgraph child["namespace: k3k-sandbox"]
        NS["Namespace<br/>managed-by=cluster-registrar<br/>cluster=sandbox<br/>flux=true"]
        KC["Secret: k3k-sandbox-kubeconfig<br/>written by the provisioner"]
    end

    REG(["argocd-cluster-registrar<br/>controller"])

    subgraph argo["namespace: argocd"]
        CS["Secret: cluster-sandbox<br/>secret-type=cluster<br/>flux=true"]
    end

    APPSET["ApplicationSet<br/>cluster generator"]

    NS -->|"1. watch by label"| REG
    KC -->|"2. read kubeconfig"| REG
    REG -->|"3. create or update, never take over"| CS
    REG -.->|"4. delete once the namespace is gone<br/>demote once the cluster is renamed"| CS
    CS -->|"selected by"| APPSET
```

The provisioner writes the kubeconfig. The registrar reshapes its credentials
into ArgoCD's format, copies across any prefixed labels from the namespace, and
writes the result into `argocd`.

Registration and removal follow namespace events, so they happen about as fast as
the API server delivers one. Every cluster is then revisited on a timer as well,
which is what keeps credentials fresh across a certificate rotation:

```mermaid
stateDiagram-v2
    direction LR
    [*] --> Waiting: namespace labelled
    Waiting --> Registered: kubeconfig Secret appears
    Waiting --> Waiting: provisioner still booting
    Waiting --> Refused: name held by another namespace
    Refused --> Registered: the holder goes away
    Registered --> Registered: kubeconfig re-read every interval<br/>(requeue; survives cert rotation)
    Registered --> Demoted: cluster label renamed<br/>hidden from ArgoCD, kept intact
    Demoted --> Registered: rename reverted
    Registered --> Pinned: prune=disabled
    Pinned --> Registered: label removed
    Registered --> [*]: source namespace deleted<br/>cluster Secret removed
    Demoted --> [*]: source namespace deleted
```

Cluster `Secret`s that carry the ownership label but whose source namespace has
gone are deleted. Anything without that label is left alone, so clusters you
registered by hand are safe. To pin one that this tool *does* own, label it
`<labelPrefix>prune: disabled` and neither deletion nor demotion will touch it.

Renaming a cluster is the one other way a registration leaves ArgoCD, and it is
not a deletion. The new name is registered and the old `Secret` is **demoted**:
ArgoCD's `secret-type` label is parked under `<labelPrefix>orphaned-secret-type`,
alongside `<labelPrefix>superseded-by` and `<labelPrefix>stale-since`. ArgoCD
finds clusters by that one label, so the stale entry disappears at once while
nothing is destroyed. Change the label back and the registration returns intact,
so a mistaken rename costs nothing.

RBAC is split by scope. Reads are cluster-wide (`namespaces` get/list/watch,
`secrets` **list only**) because discovery is label-driven and the sources sit in
one namespace per child. Every **write** is a namespaced `Role` bound to
`targetNamespace` alone, since that is the only place this ever creates, updates
or deletes anything. Granting `secrets` write across the whole cluster would be a
privilege-escalation path in exchange for nothing. Note `watch` is granted on
namespaces only: see Architecture below for why the kubeconfig `Secret`s are read
rather than watched.

## Operating

### Operational surface

A controller, so: `/healthz` and `/readyz` on `:8081` by default, tunable under
`probes`. Readiness means the manager is running, not that this replica holds the
lease, so a standby stays Ready.

Metrics are **not** served. Enabling them would mean either an unauthenticated
port or pulling in the API server authn/authz stack, and there is nothing here
worth either yet.

`leaderElection` is off by default and needs `leases` and `events` in
`targetNamespace`. The lease is named for `labelPrefix` and `managedBy`, not for
the release, because those are what decide whether two installs collide at all --
so two releases that would fight for the same `Secret`s contend for the same
lease, and two that never would are left alone.

`interval` is a **requeue** period, not a poll. Registration and removal follow
namespace events; the interval only bounds how stale a credential can get.

### Running it without installing anything

`--once` performs a single sweep and exits. It never builds a manager, never
takes a lease, and falls back to your own kubeconfig, so it is safe to point at a
live cluster from a laptop:

```bash
argocd-cluster-registrar --once --dry-run --debug
```

That prints every decision it would make, including refusals, without writing
anything. It is the quickest way to see what this would do to an existing
cluster, and the easiest way to reproduce a decision the running controller made
without disturbing it. `--dry-run` also disables leader election, so a pre-flight
check can never take the running instance offline.

### Architecture

Two decisions here are deliberate and look like oversights.

**Only namespaces are watched.** Not the provisioner-written kubeconfig
`Secret`s, even though watching them would spot a credential rotation sooner.
k3k regenerates the child's keypair on *every one of its own reconciles*, so
that `Secret` changes far more often than the credential meaningfully does. The
interval is what keeps that from becoming a write per k3k reconcile against a
credential-bearing `Secret` in the ArgoCD namespace, each of which invalidates
ArgoCD's own cluster cache. Such a watch could not be narrowed either: the
provisioner owns that `Secret`, so it carries none of our labels, which is the
same reason discovery is driven by the namespace in the first place.

**Nothing is read through the controller's cache.** Every read goes direct. The
namespace existence proof in particular must not be cached, because a
label-filtered cache reports an object that stops matching the selector as a
deletion -- so a cached `NotFound` cannot tell a deleted namespace from one that
merely lost a label, and deregistering on the second would be catastrophic. The
cache is configured to error rather than silently start an informer for anything
unexpected, and no `Secret` is ever held in it.

### Who is allowed to set these labels

The two labels are **policy input**, not decoration: together they decide whether
a cluster is registered at all and what name it takes in `argocd`, where writing
a cluster `Secret` is an administrative act. Treat them as the platform
operator's to set.

Kubernetes helps by default: the built-in `admin` role, bound into a namespace
with a `RoleBinding`, grants no write access to the `Namespace` object itself, so
an ordinary tenant cannot relabel their own namespace. But **whoever can create a
namespace sets its labels at creation**, so a cluster where teams self-serve
namespaces is a different situation.

Incumbency stops a registration being taken. It cannot stop someone who can label
a namespace from registering a cluster under any *free* name, and no collision
rule could: the label is the authorization. If you cannot vouch for who sets
these labels, constrain them where they are written, with a
`ValidatingAdmissionPolicy` binding permitted cluster names to namespace metadata.

### Changing `managedBy` or `labelPrefix` later

Both are part of the ownership record written onto every cluster `Secret`, so
changing either on a running install orphans everything already registered. The
new instance refuses to adopt those `Secret`s, and garbage collection will not see
them either, since it selects on the same label.

Neither is meant to change, but if you must: delete the old cluster `Secret`s and
let them be recreated, or relabel them by hand to the new values first. The
refusal is logged per cluster, naming the `Secret` and the namespace.
