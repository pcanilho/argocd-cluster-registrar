package registrar

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"

	coreV1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// DefaultLabelPrefix namespaces every label this tool reads and writes.
	// Deliberately the project's own name rather than any particular user's
	// domain, so the tool is usable as shipped; override with --label-prefix to
	// fit an existing labelling convention. A single DNS label is a valid
	// label-key prefix, so this needs no dots.
	DefaultLabelPrefix = "argocd-cluster-registrar/"

	// SuffixManagedBy marks BOTH the source namespaces this tool discovers and the
	// ArgoCD cluster Secrets it owns. Garbage collection only ever deletes Secrets
	// carrying it, so a hand-registered cluster is never touched.
	SuffixManagedBy = "managed-by"

	// SuffixCluster names the child cluster on the source namespace.
	SuffixCluster = "cluster"

	// argoSecretTypeLabel is what makes ArgoCD treat a Secret as a cluster.
	argoSecretTypeLabel = "argocd.argoproj.io/secret-type"
	argoSecretTypeValue = "cluster"
)

// ManagedByLabel is the discovery and ownership label key for a given prefix.
func ManagedByLabel(prefix string) string { return prefix + SuffixManagedBy }

// ClusterLabel is the cluster-name label key for a given prefix.
func ClusterLabel(prefix string) string { return prefix + SuffixCluster }

// Config controls a single reconcile pass.
type Config struct {
	// TargetNamespace is where ArgoCD reads cluster Secrets from. ArgoCD only
	// ever looks in its OWN namespace, so this is effectively always "argocd".
	TargetNamespace string

	// ManagedByValue is the ownership marker. Namespaces labelled with it are
	// discovered; cluster Secrets labelled with it are eligible for GC.
	ManagedByValue string

	// SecretNamePattern matches the kubeconfig Secret within a discovered
	// namespace, e.g. "k3k-*-kubeconfig".
	//
	// Matching by NAME rather than by label is not a stylistic choice: k3k creates
	// that Secret itself (with an ownerReference to the Cluster), so it carries
	// none of our labels and there is nowhere to add them. The namespace is the
	// nearest object we own, which is why intent lives there instead.
	SecretNamePattern string

	// SecretKey is the kubeconfig key inside that Secret. k3k uses
	// "kubeconfig.yaml"; vcluster uses "config"; CAPI uses "value".
	SecretKey string

	// LabelPrefix namespaces the labels read from the source namespace and copied
	// onto the ArgoCD cluster Secret. Defaults to DefaultLabelPrefix.
	LabelPrefix string

	// DryRun logs what would change without writing anything.
	DryRun bool
}

// Registrar reconciles child-cluster kubeconfigs into ArgoCD cluster Secrets.
type Registrar struct {
	client kubernetes.Interface
	cfg    Config
	log    *slog.Logger
}

// New builds a Registrar against the ambient cluster (in-cluster config when
// running as a Deployment, otherwise the caller's kubeconfig).
func New(log *slog.Logger, cfg Config) (*Registrar, error) {
	restCfg, err := restConfig()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	if cfg.LabelPrefix == "" {
		cfg.LabelPrefix = DefaultLabelPrefix
	}
	return &Registrar{client: client, cfg: cfg, log: log}, nil
}

func (r *Registrar) managedByLabel() string { return ManagedByLabel(r.cfg.LabelPrefix) }
func (r *Registrar) clusterLabel() string   { return ClusterLabel(r.cfg.LabelPrefix) }

func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("no in-cluster config and no usable kubeconfig: %w", err)
	}
	return cfg, nil
}

// Reconcile performs one full pass: every discovered child is registered or
// updated, and every owned Secret without a live source is removed.
//
// It is deliberately a full reconcile rather than an event-driven diff. Besides
// being far simpler to reason about, it re-reads each kubeconfig every pass,
// which is what keeps registrations valid across a k3s server restart -- those
// rotate the client certificate, and a stale Secret breaks every Application
// targeting that cluster with an authentication error rather than a visible one.
func (r *Registrar) Reconcile(ctx context.Context) error {
	desired, err := r.discover(ctx)
	if err != nil {
		return err
	}

	var errs []string
	for _, d := range desired {
		if err := r.apply(ctx, d); err != nil {
			// One bad child must not stop the others, or a single broken cluster
			// would stall registration for the whole fleet.
			errs = append(errs, fmt.Sprintf("%s: %v", d.cluster, err))
			r.log.Error("failed to register cluster", slog.String("cluster", d.cluster), slog.Any("error", err))
		}
	}

	if err := r.collect(ctx, desired); err != nil {
		errs = append(errs, fmt.Sprintf("gc: %v", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("reconcile completed with errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// child is one discovered cluster ready to be written out.
type child struct {
	cluster string
	server  string
	config  string
	labels  map[string]string
}

func (r *Registrar) discover(ctx context.Context) ([]child, error) {
	selector := fmt.Sprintf("%s=%s", r.managedByLabel(), r.cfg.ManagedByValue)
	namespaces, err := r.client.CoreV1().Namespaces().List(ctx, metaV1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list namespaces (%s): %w", selector, err)
	}

	var out []child
	for i := range namespaces.Items {
		ns := &namespaces.Items[i]

		// A namespace being torn down still lists; registering from it would
		// re-create a Secret that GC just removed.
		if ns.DeletionTimestamp != nil {
			r.log.Debug("skipping terminating namespace", slog.String("namespace", ns.Name))
			continue
		}

		name := ns.Labels[r.clusterLabel()]
		if name == "" {
			r.log.Warn("namespace is managed but has no cluster label; skipping",
				slog.String("namespace", ns.Name), slog.String("label", r.clusterLabel()))
			continue
		}

		secret, err := r.findKubeconfigSecret(ctx, ns.Name)
		if err != nil {
			// Entirely normal between the Cluster CR being created and its server
			// becoming ready -- k3k only writes the kubeconfig once the API is up.
			r.log.Info("no kubeconfig secret yet; will retry",
				slog.String("namespace", ns.Name), slog.String("cluster", name), slog.Any("reason", err))
			continue
		}

		// findKubeconfigSecret already guarantees the key is present.
		raw := secret.Data[r.cfg.SecretKey]

		server, config, err := parseKubeconfig(raw)
		if err != nil {
			r.log.Warn("unparseable kubeconfig; skipping",
				slog.String("secret", secret.Name), slog.Any("error", err))
			continue
		}

		out = append(out, child{
			cluster: name,
			server:  server,
			config:  config,
			labels:  propagatedLabels(ns.Labels, r.cfg.LabelPrefix),
		})
	}

	// Stable ordering keeps logs diffable between passes.
	sort.Slice(out, func(i, j int) bool { return out[i].cluster < out[j].cluster })
	return out, nil
}

// findKubeconfigSecret returns the first Secret in ns whose name matches the
// configured glob AND which actually carries the configured key.
//
// Both conditions matter. A name-only match picks the wrong object whenever a
// provisioner writes more than one Secret under the same prefix: vcluster's
// `vc-*` matches both `vc-<name>` (the kubeconfig) and `vc-config-<name>` (the
// vcluster config), and the latter sorts first. Requiring the key means the
// alphabetically-earlier decoy is skipped instead of poisoning the namespace.
func (r *Registrar) findKubeconfigSecret(ctx context.Context, ns string) (*coreV1.Secret, error) {
	secrets, err := r.client.CoreV1().Secrets(ns).List(ctx, metaV1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}

	// Deterministic order so repeated passes pick the same Secret when several
	// are equally valid.
	idx := make([]int, 0, len(secrets.Items))
	for i := range secrets.Items {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool {
		return secrets.Items[idx[a]].Name < secrets.Items[idx[b]].Name
	})

	var namedButKeyless []string
	for _, i := range idx {
		s := &secrets.Items[i]
		ok, err := path.Match(r.cfg.SecretNamePattern, s.Name)
		if err != nil {
			return nil, fmt.Errorf("bad secret name pattern %q: %w", r.cfg.SecretNamePattern, err)
		}
		if !ok {
			continue
		}
		if _, hasKey := s.Data[r.cfg.SecretKey]; !hasKey {
			namedButKeyless = append(namedButKeyless, s.Name)
			continue
		}
		return s, nil
	}

	if len(namedButKeyless) > 0 {
		return nil, fmt.Errorf("secret(s) %v match %q but none carry key %q",
			namedButKeyless, r.cfg.SecretNamePattern, r.cfg.SecretKey)
	}
	return nil, fmt.Errorf("no secret matching %q", r.cfg.SecretNamePattern)
}

// propagatedLabels copies the prefixed labels the ApplicationSet selects on,
// minus the ones that describe the source rather than the cluster.
func propagatedLabels(in map[string]string, prefix string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if k == ManagedByLabel(prefix) || k == ClusterLabel(prefix) {
			continue
		}
		out[k] = v
	}
	return out
}

// secretName is the ArgoCD cluster Secret name for a child.
func secretName(cluster string) string { return "cluster-" + cluster }

func (r *Registrar) apply(ctx context.Context, c child) error {
	labels := map[string]string{
		argoSecretTypeLabel: argoSecretTypeValue,
		r.managedByLabel():  r.cfg.ManagedByValue,
		r.clusterLabel():    c.cluster,
	}
	for k, v := range c.labels {
		labels[k] = v
	}

	want := &coreV1.Secret{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      secretName(c.cluster),
			Namespace: r.cfg.TargetNamespace,
			Labels:    labels,
		},
		Type: coreV1.SecretTypeOpaque,
		StringData: map[string]string{
			"name":   c.cluster,
			"server": c.server,
			"config": c.config,
		},
	}

	api := r.client.CoreV1().Secrets(r.cfg.TargetNamespace)
	existing, err := api.Get(ctx, want.Name, metaV1.GetOptions{})
	switch {
	case apiErrors.IsNotFound(err):
		if r.cfg.DryRun {
			r.log.Info("[dry-run] would create cluster secret",
				slog.String("cluster", c.cluster), slog.String("server", c.server))
			return nil
		}
		if _, err := api.Create(ctx, want, metaV1.CreateOptions{}); err != nil {
			return fmt.Errorf("create: %w", err)
		}
		r.log.Info("registered cluster",
			slog.String("cluster", c.cluster), slog.String("server", c.server))
		return nil

	case err != nil:
		return fmt.Errorf("get: %w", err)
	}

	if !changed(existing, want, r.cfg.LabelPrefix) {
		r.log.Debug("cluster secret already up to date", slog.String("cluster", c.cluster))
		return nil
	}
	if r.cfg.DryRun {
		r.log.Info("[dry-run] would update cluster secret", slog.String("cluster", c.cluster))
		return nil
	}

	// Carry resourceVersion so a concurrent writer produces a conflict rather
	// than a silent overwrite; the next pass retries.
	want.ResourceVersion = existing.ResourceVersion
	if _, err := api.Update(ctx, want, metaV1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	r.log.Info("updated cluster registration",
		slog.String("cluster", c.cluster), slog.String("server", c.server))
	return nil
}

// changed reports whether the live Secret differs from what we would write.
// Compares Data (what the apiserver stores) against StringData (what we set).
func changed(existing, want *coreV1.Secret, labelPrefix string) bool {
	for k, v := range want.StringData {
		if string(existing.Data[k]) != v {
			return true
		}
	}
	for k, v := range want.Labels {
		if existing.Labels[k] != v {
			return true
		}
	}
	// A label removed from the source must disappear here too, or a cluster could
	// never be opted OUT of Flux once opted in.
	for k := range existing.Labels {
		if strings.HasPrefix(k, labelPrefix) {
			if _, ok := want.Labels[k]; !ok {
				return true
			}
		}
	}
	return false
}

// collect deletes ArgoCD cluster Secrets this tool owns whose child no longer
// exists. Without it a destroyed cluster leaves a permanently broken entry in
// ArgoCD -- the original tool created Secrets but never removed them, which is
// the single most important behaviour added here.
func (r *Registrar) collect(ctx context.Context, desired []child) error {
	live := map[string]bool{}
	for _, d := range desired {
		live[secretName(d.cluster)] = true
	}

	selector := fmt.Sprintf("%s=%s,%s=%s",
		argoSecretTypeLabel, argoSecretTypeValue,
		r.managedByLabel(), r.cfg.ManagedByValue)

	secrets, err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).List(ctx, metaV1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list cluster secrets (%s): %w", selector, err)
	}

	for i := range secrets.Items {
		s := &secrets.Items[i]
		if live[s.Name] {
			continue
		}
		if r.cfg.DryRun {
			r.log.Info("[dry-run] would delete orphaned cluster secret", slog.String("secret", s.Name))
			continue
		}
		if err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).Delete(ctx, s.Name, metaV1.DeleteOptions{}); err != nil {
			if apiErrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("delete %s: %w", s.Name, err)
		}
		r.log.Info("deregistered cluster (source gone)", slog.String("secret", s.Name))
	}
	return nil
}
