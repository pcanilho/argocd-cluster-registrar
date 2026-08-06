package registrar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	coreV1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
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

	// SuffixSourceNamespace records, on the ArgoCD cluster Secret, which
	// namespace a registration came from. Garbage collection needs it to confirm
	// the source is genuinely gone rather than merely unseen this pass.
	SuffixSourceNamespace = "source-namespace"

	// argoSecretTypeLabel is what makes ArgoCD treat a Secret as a cluster.
	argoSecretTypeLabel = "argocd.argoproj.io/secret-type"
	argoSecretTypeValue = "cluster"
)

// ManagedByLabel is the discovery and ownership label key for a given prefix.
func ManagedByLabel(prefix string) string { return prefix + SuffixManagedBy }

// ClusterLabel is the cluster-name label key for a given prefix.
func ClusterLabel(prefix string) string { return prefix + SuffixCluster }

// SourceNamespaceLabel is the source-namespace label key for a given prefix.
func SourceNamespaceLabel(prefix string) string { return prefix + SuffixSourceNamespace }

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

// Validate checks configuration that would otherwise fail late, per-namespace,
// or silently. Called before the client is built so a bad flag stops the process
// at startup instead of every 60 seconds forever.
func (c Config) Validate() error {
	if c.TargetNamespace == "" {
		// Secrets("") lists across ALL namespaces, so this would silently widen
		// GC's search far beyond where the tool ever writes.
		return fmt.Errorf("--target-namespace must not be empty")
	}
	if c.ManagedByValue == "" {
		return fmt.Errorf("--managed-by must not be empty")
	}
	if c.SecretKey == "" {
		return fmt.Errorf("--secret-key must not be empty")
	}
	if c.SecretNamePattern == "" {
		return fmt.Errorf("--secret-name-pattern must not be empty")
	}
	// A malformed glob is a permanent fault. It used to surface once per
	// namespace per pass, disguised as "no kubeconfig secret yet".
	if _, err := path.Match(c.SecretNamePattern, "probe"); err != nil {
		return fmt.Errorf("--secret-name-pattern %q is not a valid glob: %w", c.SecretNamePattern, err)
	}
	if p := c.LabelPrefix; p != "" && !strings.HasSuffix(p, "/") {
		// Without the slash the keys become nonsense like "example.commanaged-by"
		// while propagatedLabels still matches "example.com/...", so it half-works.
		return fmt.Errorf("--label-prefix %q must end in '/'", p)
	}
	if c.LabelPrefix != "" {
		if errs := validation.IsQualifiedName(ManagedByLabel(c.LabelPrefix)); len(errs) > 0 {
			return fmt.Errorf("--label-prefix %q does not yield a valid label key: %s",
				c.LabelPrefix, strings.Join(errs, "; "))
		}
	}
	return nil
}

func (r *Registrar) managedByLabel() string       { return ManagedByLabel(r.cfg.LabelPrefix) }
func (r *Registrar) clusterLabel() string         { return ClusterLabel(r.cfg.LabelPrefix) }
func (r *Registrar) sourceNamespaceLabel() string { return SourceNamespaceLabel(r.cfg.LabelPrefix) }

// clientTimeout bounds every API call. Without it rest.Config.Timeout is 0, so a
// hung request inside Reconcile blocks forever: the ticker never fires again and
// the pod sits Running and Ready while doing nothing at all. Nothing else in the
// stack detects that, because the process has not crashed.
const clientTimeout = 30 * time.Second

func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		cfg.Timeout = clientTimeout
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("no in-cluster config and no usable kubeconfig: %w", err)
	}
	cfg.Timeout = clientTimeout
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
	desired, complete, err := r.discover(ctx)
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

	// Garbage collection runs ONLY on a complete picture. `desired` is built by
	// skipping namespaces that could not be evaluated, so a transient API error
	// makes a live cluster look deleted. Without this guard one 500 from the
	// apiserver would deregister the entire fleet, break every Application
	// pointing at it, and re-create the lot on the next pass.
	if !complete {
		r.log.Warn("skipping garbage collection: could not evaluate every managed namespace this pass")
	} else if err := r.collect(ctx, desired); err != nil {
		errs = append(errs, fmt.Sprintf("gc: %v", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("reconcile completed with errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// child is one discovered cluster ready to be written out.
type child struct {
	cluster   string
	namespace string
	server    string
	config    string
	labels    map[string]string
}

// apiFailure marks an error as "the API call failed", as opposed to "the answer
// is legitimately not there yet". Only the former makes the view incomplete.
type apiFailure struct{ err error }

func (e *apiFailure) Error() string { return e.err.Error() }
func (e *apiFailure) Unwrap() error { return e.err }

// discover returns the clusters that should be registered, and whether every
// managed namespace was successfully evaluated.
//
// The bool is load-bearing: callers must not treat "absent from the result" as
// "deleted" unless it is true. A namespace that could not be read is omitted
// from the result but clears the flag.
func (r *Registrar) discover(ctx context.Context) ([]child, bool, error) {
	selector := fmt.Sprintf("%s=%s", r.managedByLabel(), r.cfg.ManagedByValue)
	namespaces, err := r.client.CoreV1().Namespaces().List(ctx, metaV1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, false, fmt.Errorf("list namespaces (%s): %w", selector, err)
	}

	complete := true
	var out []child
	seen := map[string]string{}
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
			complete = false
			continue
		}

		// The name becomes a Secret name, so it must be a valid DNS-1123
		// subdomain. Label values legally allow uppercase, "_" and ".", none of
		// which are, and the apiserver would otherwise reject the write on every
		// pass forever.
		if errs := validation.IsDNS1123Subdomain(secretName(name)); len(errs) > 0 {
			r.log.Error("cluster name does not yield a valid Secret name; skipping",
				slog.String("namespace", ns.Name), slog.String("cluster", name),
				slog.String("reason", strings.Join(errs, "; ")))
			complete = false
			continue
		}

		// Two namespaces claiming one cluster name would map to a single Secret
		// and flap between them forever. Skip both rather than pick a winner.
		if other, dup := seen[name]; dup {
			r.log.Error("duplicate cluster name across namespaces; skipping both",
				slog.String("cluster", name), slog.String("namespace", ns.Name),
				slog.String("conflictsWith", other))
			complete = false
			out = slices.DeleteFunc(out, func(c child) bool { return c.cluster == name })
			continue
		}
		seen[name] = ns.Name

		secret, err := r.findKubeconfigSecret(ctx, ns.Name)
		if err != nil {
			var apiErr *apiFailure
			if errors.As(err, &apiErr) {
				// A genuine API failure, NOT "the provisioner has not written it
				// yet". Logging this as routine is how a fleet-wide deregistration
				// used to look reassuring in the logs.
				r.log.Error("could not read secrets in managed namespace",
					slog.String("namespace", ns.Name), slog.String("cluster", name), slog.Any("error", err))
				complete = false
				continue
			}
			// Entirely normal between the Cluster CR being created and its server
			// becoming ready -- k3k only writes the kubeconfig once the API is up.
			r.log.Info("no kubeconfig secret yet; will retry",
				slog.String("namespace", ns.Name), slog.String("cluster", name), slog.Any("reason", err))
			complete = false
			continue
		}

		// findKubeconfigSecret already guarantees the key is present.
		raw := secret.Data[r.cfg.SecretKey]

		server, config, err := parseKubeconfig(raw)
		if err != nil {
			// Could be a half-written kubeconfig, so this must not look like a
			// deletion either.
			r.log.Warn("unparseable kubeconfig; skipping",
				slog.String("secret", secret.Name), slog.Any("error", err))
			complete = false
			continue
		}

		out = append(out, child{
			cluster:   name,
			namespace: ns.Name,
			server:    server,
			config:    config,
			labels:    propagatedLabels(ns.Labels, r.cfg.LabelPrefix),
		})
	}

	// Stable ordering keeps logs diffable between passes.
	sort.Slice(out, func(i, j int) bool { return out[i].cluster < out[j].cluster })
	return out, complete, nil
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
		return nil, &apiFailure{fmt.Errorf("list secrets: %w", err)}
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
			// A permanent configuration fault, not a transient one. New()
			// validates the pattern up front so this should be unreachable.
			return nil, &apiFailure{fmt.Errorf("bad secret name pattern %q: %w", r.cfg.SecretNamePattern, err)}
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
		argoSecretTypeLabel:      argoSecretTypeValue,
		r.managedByLabel():       r.cfg.ManagedByValue,
		r.clusterLabel():         c.cluster,
		r.sourceNamespaceLabel(): c.namespace,
	}
	maps.Copy(labels, c.labels)

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

	// Read-modify-write, NOT a wholesale replace. Update replaces the entire
	// object, so sending a freshly-built Secret would silently drop everything
	// this tool does not know about: ArgoCD's own `namespaces`,
	// `clusterResources` and `project` keys, plus any annotations or foreign
	// labels an operator set by hand. changed() cannot see that drift either, so
	// the loss would land later, at an unrelated update, with no log line.
	updated := existing.DeepCopy()
	if updated.Data == nil {
		updated.Data = map[string][]byte{}
	}
	for k, v := range want.StringData {
		updated.Data[k] = []byte(v)
	}
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	maps.Copy(updated.Labels, want.Labels)
	// Drop prefixed labels that no longer exist upstream, so a cluster can be
	// opted back OUT of a selector (e.g. flux=true) once it was opted in.
	for k := range updated.Labels {
		if strings.HasPrefix(k, r.cfg.LabelPrefix) {
			if _, keep := want.Labels[k]; !keep {
				delete(updated.Labels, k)
			}
		}
	}
	if _, err := api.Update(ctx, updated, metaV1.UpdateOptions{}); err != nil {
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
// ArgoCD -- the original tool created Secrets but never removed them.
//
// Deletion requires POSITIVE proof, not absence from `desired`. Each owned
// Secret records the namespace it came from, and that namespace must return a
// definite NotFound before anything is removed. Absence alone is not evidence:
// a namespace can drop out of `desired` because an API call failed, because a
// kubeconfig was half-written, or because a label was edited, and treating any
// of those as "deleted" would deregister live clusters. Reconcile also refuses
// to call this at all on an incomplete pass; this is the second line of defence.
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

	var errs []string
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if live[s.Name] {
			continue
		}

		srcNS := s.Labels[r.sourceNamespaceLabel()]
		if srcNS == "" {
			// Written by an older version, before the source was recorded. There
			// is no way to prove the source is gone, so leave it alone and say so
			// rather than guessing.
			r.log.Warn("owned cluster secret has no source-namespace label; not deleting",
				slog.String("secret", s.Name), slog.String("label", r.sourceNamespaceLabel()))
			continue
		}

		if _, err := r.client.CoreV1().Namespaces().Get(ctx, srcNS, metaV1.GetOptions{}); err == nil {
			// Still there. Something else kept it out of `desired`, so this is a
			// problem to report, never a deletion.
			r.log.Warn("source namespace still exists but produced no registration; not deleting",
				slog.String("secret", s.Name), slog.String("namespace", srcNS))
			continue
		} else if !apiErrors.IsNotFound(err) {
			errs = append(errs, fmt.Sprintf("%s: confirm namespace %s: %v", s.Name, srcNS, err))
			continue
		}

		if r.cfg.DryRun {
			r.log.Info("[dry-run] would delete orphaned cluster secret",
				slog.String("secret", s.Name), slog.String("namespace", srcNS))
			continue
		}
		if err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).Delete(ctx, s.Name, metaV1.DeleteOptions{}); err != nil {
			if apiErrors.IsNotFound(err) {
				continue
			}
			// Keep going. One Secret held up by a finalizer or an admission
			// webhook must not stall GC for the whole fleet, which is the same
			// reasoning the apply loop uses.
			errs = append(errs, fmt.Sprintf("delete %s: %v", s.Name, err))
			continue
		}
		r.log.Info("deregistered cluster (source namespace gone)",
			slog.String("secret", s.Name), slog.String("namespace", srcNS))
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
