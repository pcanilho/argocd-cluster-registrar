package registrar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"sort"
	"strings"
	"time"

	coreV1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
	// the source is genuinely gone rather than merely unseen this pass. It is
	// also what apply() checks to refuse taking over another namespace's
	// registration, so it is the ownership record, not merely a breadcrumb.
	SuffixSourceNamespace = "source-namespace"

	// SuffixSourceNamespaceUID records that namespace's UID. Names are reusable
	// and UIDs are not, so this is what tells "the same namespace" apart from "a
	// new namespace wearing the old one's name".
	//
	// Written but not yet read. Two things want it: a delete precondition, so a
	// garbage collection decided in one pass cannot land on a registration a
	// later pass recreated; and the existence proof in collect(), which today
	// looks the namespace up by name and so cannot tell a survivor from a
	// replacement. Both are follow-ups; stamping it now means the data is
	// already there when they land.
	SuffixSourceNamespaceUID = "source-namespace-uid"

	// SuffixProvider records which configured provider matched, so a fleet is
	// introspectable and an ApplicationSet can select by provisioner.
	SuffixProvider = "provider"

	// argoSecretTypeLabel is what makes ArgoCD treat a Secret as a cluster. It is
	// a label key, not a credential; gosec G101 matches on the identifier holding
	// "Secret" next to a string literal.
	argoSecretTypeLabel = "argocd.argoproj.io/secret-type" // #nosec G101
	argoSecretTypeValue = "cluster"
)

// ManagedByLabel is the discovery and ownership label key for a given prefix.
func ManagedByLabel(prefix string) string { return prefix + SuffixManagedBy }

// ClusterLabel is the cluster-name label key for a given prefix.
func ClusterLabel(prefix string) string { return prefix + SuffixCluster }

// SourceNamespaceLabel is the source-namespace label key for a given prefix.
func SourceNamespaceLabel(prefix string) string { return prefix + SuffixSourceNamespace }

// SourceNamespaceUIDLabel is the source-namespace-uid label key for a given prefix.
func SourceNamespaceUIDLabel(prefix string) string { return prefix + SuffixSourceNamespaceUID }

// ProviderLabel is the matched-provider label key for a given prefix.
func ProviderLabel(prefix string) string { return prefix + SuffixProvider }

// reservedSuffixes are the labels this tool derives itself. They are written on
// every cluster Secret from what we observed, never copied from the source
// namespace -- otherwise a namespace could set them and lie about its own
// registration. `source-namespace` is the dangerous one: it is the pointer
// garbage collection follows to prove a source is gone, so a namespace labelling
// itself `<prefix>source-namespace: kube-system` would aim that proof at a
// namespace that never disappears and make its registration immortal.
var reservedSuffixes = []string{
	SuffixManagedBy,
	SuffixCluster,
	SuffixSourceNamespace,
	SuffixSourceNamespaceUID,
	SuffixProvider,
}

// Provider is one provisioner's kubeconfig Secret shape.
//
// The pattern and the keys travel together as a unit, and that pairing is
// load-bearing rather than tidy: it is what tells a real kubeconfig apart from a
// decoy sitting under the same prefix. vcluster's `vc-*` matches both `vc-<name>`
// (key `config`) and `vc-config-<name>` (key `config.yaml`). Whether the decoy
// sorts first depends on the names -- `vc-config-x` precedes `vc-x`, but
// `vc-config-abc` follows `vc-abc` -- so the key is the only reliable
// discriminator. Two independent lists of patterns and keys would lose that.
type Provider struct {
	// Name identifies the provider in logs and on the cluster Secret's provider
	// label. It is not matched against anything.
	Name string

	// SecretNamePattern is a glob matched against Secret names within a
	// discovered namespace, e.g. "k3k-*-kubeconfig".
	//
	// Matching by NAME rather than by label is not a stylistic choice: the
	// provisioner creates that Secret itself (k3k gives it an ownerReference to
	// the Cluster), so it carries none of our labels and there is nowhere to add
	// them. The namespace is the nearest object we own, which is why intent lives
	// there instead.
	SecretNamePattern string

	// SecretKeys are the keys that may hold the kubeconfig, tried in order. A
	// list rather than a single key because one provisioner can legitimately use
	// either: Kamaji writes `admin.conf`, or `admin.svc` when its control plane
	// advertises a service address.
	SecretKeys []string
}

// presets are the provisioner shapes shipped with the tool, so that configuring
// one is `providers: [k3k, capi]` rather than a glob a user has to get right.
//
// Only k3k, vcluster and kamaji have been run against the real thing; see the
// provisioner table in README.md, which distinguishes tested from assumed and
// should be kept honest.
//
// Every entry carries `#nosec G101`. gosec matches that rule on identifiers like
// `Secret*` sitting next to a string literal, which describes this map exactly --
// but `SecretNamePattern` is a glob matched against Secret NAMES and `SecretKeys`
// are map keys inside a Secret. Neither is a credential, and nothing here is ever
// compared against one. The same false positive is already annotated on
// argoSecretTypeLabel above.
var presets = map[string]Provider{
	"k3k": { // #nosec G101 -- a Secret name glob and a data key, not a credential
		Name:              "k3k",
		SecretNamePattern: "k3k-*-kubeconfig",
		SecretKeys:        []string{"kubeconfig.yaml"},
	},
	"vcluster": { // #nosec G101 -- see above
		Name:              "vcluster",
		SecretNamePattern: "vc-*",
		SecretKeys:        []string{"config"},
	},
	// Kamaji's standalone shape. Note that driving Kamaji through its Cluster API
	// control-plane provider ALSO produces a second, CAPI-shaped Secret for the
	// same physical cluster -- see the note on candidate ordering in discover.
	"kamaji": { // #nosec G101 -- see above
		Name:              "kamaji",
		SecretNamePattern: "*-admin-kubeconfig",
		SecretKeys:        []string{"admin.conf", "admin.svc"},
	},
	// The Cluster API contract, which is mandatory for control-plane providers
	// rather than merely conventional: the Secret must be `<cluster>-kubeconfig`
	// in the Cluster's namespace with the kubeconfig under `value`. That makes
	// this one entry cover every CAPI cluster whatever the infrastructure
	// provider, including standalone k0smotron, which adopts the same shape.
	//
	// This is deliberately the loosest pattern shipped: `*-kubeconfig` also
	// matches k3k's Secret and anything else in a managed namespace ending that
	// way. Correctness comes from the key, not the name.
	"capi": { // #nosec G101 -- see above
		Name:              "capi",
		SecretNamePattern: "*-kubeconfig",
		SecretKeys:        []string{"value"},
	},
}

// Preset returns a built-in provider by name.
func Preset(name string) (Provider, bool) {
	p, ok := presets[name]
	return p, ok
}

// PresetNames lists the built-in providers, sorted, for help text and errors.
func PresetNames() []string {
	out := make([]string, 0, len(presets))
	for name := range presets {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ParseProvider reads one provider spec: either a preset name ("k3k"), or a
// custom shape as "name=pattern=key[,key...]".
func ParseProvider(spec string) (Provider, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Provider{}, fmt.Errorf("empty provider spec")
	}

	if !strings.Contains(spec, "=") {
		p, ok := Preset(spec)
		if !ok {
			return Provider{}, fmt.Errorf(
				"unknown provider %q: expected one of %s, or a custom spec \"name=pattern=key[,key...]\"",
				spec, strings.Join(PresetNames(), ", "))
		}
		return p, nil
	}

	parts := strings.Split(spec, "=")
	if len(parts) != 3 {
		return Provider{}, fmt.Errorf(
			"malformed provider spec %q: expected \"name=pattern=key[,key...]\"", spec)
	}

	var keys []string
	for _, k := range strings.Split(parts[2], ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}

	// Validate() re-checks all of this; returning a well-formed value here keeps
	// the error messages about spec syntax rather than about configuration.
	return Provider{
		Name:              strings.TrimSpace(parts[0]),
		SecretNamePattern: strings.TrimSpace(parts[1]),
		SecretKeys:        keys,
	}, nil
}

// Config controls a single reconcile pass.
type Config struct {
	// TargetNamespace is where ArgoCD reads cluster Secrets from. ArgoCD only
	// ever looks in its OWN namespace, so this is effectively always "argocd".
	TargetNamespace string

	// ManagedByValue is the ownership marker. Namespaces labelled with it are
	// discovered; cluster Secrets labelled with it are eligible for GC.
	ManagedByValue string

	// Providers are the provisioner shapes to look for, in precedence order.
	// Order matters because the patterns legitimately overlap: the CAPI preset's
	// `*-kubeconfig` also matches k3k's Secret.
	Providers []Provider

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
	if len(c.Providers) == 0 {
		return fmt.Errorf("at least one provider must be configured")
	}
	seen := map[string]bool{}
	for i, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider %d has no name", i)
		}
		// The name is written verbatim as a label value on every cluster Secret
		// this provider matches, so an invalid one is not a cosmetic problem: it
		// is accepted at startup and then rejected by the apiserver on EVERY
		// apply, forever, per cluster. Same reasoning as the DNS-1123 check on
		// cluster names below. The fake clientset used in tests does not validate
		// labels, so nothing else would catch it.
		if errs := validation.IsValidLabelValue(p.Name); len(errs) > 0 {
			return fmt.Errorf("provider %q: name is not a valid label value: %s",
				p.Name, strings.Join(errs, "; "))
		}
		// Two providers sharing a name would make the provider label ambiguous
		// and the logs unreadable.
		if seen[p.Name] {
			return fmt.Errorf("duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true

		if p.SecretNamePattern == "" {
			return fmt.Errorf("provider %q: secret name pattern must not be empty", p.Name)
		}
		if len(p.SecretKeys) == 0 {
			return fmt.Errorf("provider %q: at least one secret key must be set", p.Name)
		}
		for _, k := range p.SecretKeys {
			if k == "" {
				return fmt.Errorf("provider %q: secret key must not be empty", p.Name)
			}
		}
		// A malformed glob is a permanent fault. It used to surface once per
		// namespace per pass, disguised as "no kubeconfig secret yet".
		if _, err := path.Match(p.SecretNamePattern, "probe"); err != nil {
			return fmt.Errorf("provider %q: secret name pattern %q is not a valid glob: %w",
				p.Name, p.SecretNamePattern, err)
		}
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
func (r *Registrar) providerLabel() string        { return ProviderLabel(r.cfg.LabelPrefix) }

func (r *Registrar) sourceNamespaceUIDLabel() string {
	return SourceNamespaceUIDLabel(r.cfg.LabelPrefix)
}

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
	desired, unresolved, err := r.discover(ctx)
	if err != nil {
		return err
	}

	// Only the children that were actually written may vouch for their Secret.
	// Passing `desired` here instead would let a REFUSED claimant keep the
	// winner's registration marked live: once the winner's namespace was deleted,
	// the loser would go on being refused, the Secret would go on looking claimed,
	// and it would strand forever pointing at a dead cluster.
	applied := make([]child, 0, len(desired))
	var errs []string
	for _, d := range desired {
		err := r.apply(ctx, d)
		var conflict *conflictError
		switch {
		case err == nil:
			applied = append(applied, d)

		case errors.As(err, &conflict):
			// Not a failure, so it must not fail the pass: a contested name
			// persists until a human fixes it, and counting it would mark every
			// pass failed forever. Logged at Error all the same -- a silently
			// resolved conflict over a credential-bearing Secret is the hazard.
			r.log.Error("refused to register cluster", slog.String("cluster", d.cluster),
				slog.String("namespace", d.namespace), slog.Any("reason", err))
			unresolved[d.namespace] = true

		default:
			// One bad child must not stop the others, or a single broken cluster
			// would stall registration for the whole fleet.
			errs = append(errs, fmt.Sprintf("%s: %v", d.cluster, err))
			r.log.Error("failed to register cluster", slog.String("cluster", d.cluster), slog.Any("error", err))
			unresolved[d.namespace] = true
		}
	}

	// `applied` omits namespaces that could not be evaluated, could not be
	// written, or were refused, so absence from it never means "deleted". Rather
	// than skipping GC entirely -- which let one stuck namespace keep every dead
	// registration alive -- those namespaces are passed down in `unresolved` and
	// excluded individually. Every other cluster is still collected normally.
	if len(unresolved) > 0 {
		r.log.Warn("some managed namespaces produced no confirmed registration; theirs are exempt from garbage collection this pass",
			slog.Int("count", len(unresolved)))
	}
	if err := r.collect(ctx, applied, unresolved); err != nil {
		errs = append(errs, fmt.Sprintf("gc: %v", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("reconcile completed with errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// child is one discovered cluster ready to be written out.
type child struct {
	cluster      string
	namespace    string
	namespaceUID types.UID
	server       string
	config       string
	provider     string
	labels       map[string]string
}

// apiFailure marks an error as "the API call failed", as opposed to "the answer
// is legitimately not there yet". Only the former makes the view incomplete.
type apiFailure struct{ err error }

func (e *apiFailure) Error() string { return e.err.Error() }
func (e *apiFailure) Unwrap() error { return e.err }

// conflictError means the name is spoken for. It is deliberately distinct from a
// failure: nothing went wrong, the answer is "not yours".
//
// Reconcile must not count it toward the pass's error return. A contested name is
// a configuration mistake that persists until a human fixes it, so treating it as
// an error would fail every pass forever and, once there is a metric, pump it
// forever too. It is still logged at Error, because a silently resolved conflict
// over a credential-bearing Secret is the actual hazard.
type conflictError struct{ err error }

func (e *conflictError) Error() string { return e.err.Error() }
func (e *conflictError) Unwrap() error { return e.err }

func conflictf(format string, a ...any) error {
	return &conflictError{fmt.Errorf(format, a...)}
}

// discover returns the clusters that should be registered, and the set of
// managed namespaces whose registration state could not be established this pass.
//
// That set is load-bearing: callers must not treat "absent from the result" as
// "deleted" for a namespace listed in it. A namespace that could not be read is
// omitted from the result and recorded there instead.
//
// It is a set rather than the single bool this used to be, because one
// unevaluable namespace used to disable garbage collection for the entire fleet.
// A cluster stuck without a kubeconfig -- which every k3k child is for its first
// ninety seconds, and a permanently broken one is forever -- would silently keep
// every other cluster's dead registration alive.
//
// Reconcile ADDS to the set afterwards, for namespaces whose apply failed or was
// refused. Both are cases where this pass did not establish what the namespace
// owns, which is the same thing the discovery-time entries mean, so garbage
// collection must leave their registrations alone either way.
func (r *Registrar) discover(ctx context.Context) ([]child, map[string]bool, error) {
	selector := fmt.Sprintf("%s=%s", r.managedByLabel(), r.cfg.ManagedByValue)
	namespaces, err := r.client.CoreV1().Namespaces().List(ctx, metaV1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, nil, fmt.Errorf("list namespaces (%s): %w", selector, err)
	}

	unresolved := map[string]bool{}
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
			unresolved[ns.Name] = true
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
			unresolved[ns.Name] = true
			continue
		}

		// Two namespaces may claim one cluster name. That is NOT resolved here any
		// more: discovery is per-namespace, and deciding it here needed the whole
		// namespace list at once, which the eventual per-namespace reconcile
		// cannot have. apply() settles it instead -- see claimContestedName.
		//
		// Removing it also fixed a quieter bug. The name used to be claimed here,
		// BEFORE the kubeconfig lookup below, so a namespace that never produced a
		// usable kubeconfig still poisoned a healthy namespace claiming the same
		// name, and neither ever registered.
		candidates, err := r.findKubeconfigCandidates(ctx, ns.Name)
		if err != nil {
			var apiErr *apiFailure
			if errors.As(err, &apiErr) {
				// A genuine API failure, NOT "the provisioner has not written it
				// yet". Logging this as routine is how a fleet-wide deregistration
				// used to look reassuring in the logs.
				r.log.Error("could not read secrets in managed namespace",
					slog.String("namespace", ns.Name), slog.String("cluster", name), slog.Any("error", err))
				unresolved[ns.Name] = true
				continue
			}
			// Entirely normal between the Cluster CR being created and its server
			// becoming ready -- k3k only writes the kubeconfig once the API is up,
			// and reports ProvisioningFailed for roughly the first ninety seconds
			// while it does.
			r.log.Info("no kubeconfig secret yet; will retry",
				slog.String("namespace", ns.Name), slog.String("cluster", name), slog.Any("reason", err))
			unresolved[ns.Name] = true
			continue
		}

		// Try candidates in order. A shape match is not a usable kubeconfig: it
		// may be half-written, or carry an exec credential that cannot be copied
		// into an ArgoCD Secret, and in both cases the next candidate may be fine.
		var (
			server, config string
			provider       string
			parseErrs      []string
		)
		for _, c := range candidates {
			s, cfg, perr := parseKubeconfig(c.secret.Data[c.key])
			if perr != nil {
				parseErrs = append(parseErrs, fmt.Sprintf("%s[%s]: %v", c.secret.Name, c.key, perr))
				continue
			}
			server, config, provider = s, cfg, c.provider
			if len(parseErrs) > 0 {
				r.log.Debug("skipped unusable kubeconfig candidates before this one",
					slog.String("namespace", ns.Name), slog.String("using", c.secret.Name),
					slog.Any("skipped", parseErrs))
			}
			break
		}
		if provider == "" {
			// Could be a half-written kubeconfig, so this must not look like a
			// deletion either.
			r.log.Warn("no usable kubeconfig; skipping",
				slog.String("namespace", ns.Name), slog.String("cluster", name),
				slog.Any("errors", parseErrs))
			unresolved[ns.Name] = true
			continue
		}

		out = append(out, child{
			cluster:      name,
			namespace:    ns.Name,
			namespaceUID: ns.UID,
			server:       server,
			config:       config,
			provider:     provider,
			labels:       propagatedLabels(ns.Labels, r.cfg.LabelPrefix),
		})
	}

	// Stable ordering keeps logs diffable between passes. Two children can now
	// share a cluster name, and sort.Slice is not stable, so the namespace breaks
	// the tie -- for READABILITY only. Nothing may depend on this order: which
	// claimant wins a contested name is decided by claimContestedName, on the
	// namespace's creation timestamp, precisely so that it does not vary with the
	// order a driver happens to present namespaces in.
	sort.Slice(out, func(i, j int) bool {
		if out[i].cluster != out[j].cluster {
			return out[i].cluster < out[j].cluster
		}
		return out[i].namespace < out[j].namespace
	})
	return out, unresolved, nil
}

// candidate is a Secret that matched some provider, together with the provider
// that matched and the key holding the kubeconfig.
type candidate struct {
	secret   *coreV1.Secret
	provider string
	key      string
}

// findKubeconfigCandidates returns every Secret in ns that matches a configured
// provider's glob AND carries one of that provider's keys, in the order they
// should be tried: providers in configured precedence order, then Secret name.
//
// Both conditions matter. A name-only match picks the wrong object whenever a
// provisioner writes more than one Secret under the same prefix: vcluster's
// `vc-*` matches both `vc-<name>` (the kubeconfig, key `config`) and
// `vc-config-<name>` (the vcluster config, key `config.yaml`). Requiring the key
// means the decoy is skipped rather than poisoning the namespace, whichever way
// the two happen to sort.
//
// A LIST rather than a single answer, because matching the shape is not the same
// as being usable. Cluster API's contract is satisfied by managed-cloud
// providers too, and CAPA's EKS path writes a second `<cluster>-user-kubeconfig`
// -- same glob, same `value` key -- holding an `exec` credential that cannot be
// copied into an ArgoCD Secret at all. Returning candidates lets the caller fall
// through to the next one on a parse failure, instead of relying on `k` sorting
// before `u`.
func (r *Registrar) findKubeconfigCandidates(ctx context.Context, ns string) ([]candidate, error) {
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

	var out []candidate
	var namedButKeyless []string
	claimed := map[string]bool{}

	for _, p := range r.cfg.Providers {
		for _, i := range idx {
			s := &secrets.Items[i]
			ok, err := path.Match(p.SecretNamePattern, s.Name)
			if err != nil {
				// A permanent configuration fault, not a transient one. Validate()
				// checks every pattern up front so this should be unreachable.
				return nil, &apiFailure{fmt.Errorf("provider %q: bad secret name pattern %q: %w",
					p.Name, p.SecretNamePattern, err)}
			}
			if !ok {
				continue
			}
			// An earlier provider already claimed this Secret. Skip it rather than
			// offering it twice: the patterns overlap by design (`*-kubeconfig`
			// matches k3k's Secret too) and a duplicate entry would only ever
			// produce the same registration.
			if claimed[s.Name] {
				continue
			}

			key := ""
			for _, k := range p.SecretKeys {
				if _, has := s.Data[k]; has {
					key = k
					break
				}
			}
			if key == "" {
				namedButKeyless = append(namedButKeyless,
					fmt.Sprintf("%s (provider %s)", s.Name, p.Name))
				continue
			}

			claimed[s.Name] = true
			out = append(out, candidate{secret: s, provider: p.Name, key: key})
		}
	}

	if len(out) > 0 {
		return out, nil
	}
	// Only worth reporting when nothing matched at all. With overlapping patterns
	// a keyless near-miss is routine -- every k3k Secret is one, for the CAPI
	// provider -- and reporting it on a successful pass would turn a real signal
	// into noise.
	if len(namedButKeyless) > 0 {
		return nil, fmt.Errorf("secret(s) %v matched a provider pattern but carried none of its keys",
			namedButKeyless)
	}
	return nil, fmt.Errorf("no secret matching any configured provider")
}

// propagatedLabels copies the prefixed labels the ApplicationSet selects on,
// minus the ones this tool derives for itself.
//
// The exclusions are a trust boundary, not tidiness. Everything here comes off
// the source namespace, which a tenant may control, and apply() copies these
// over the labels it just computed -- so anything not excluded can be spoofed.
// See reservedSuffixes for what that would cost.
func propagatedLabels(in map[string]string, prefix string) map[string]string {
	reserved := map[string]bool{}
	for _, suffix := range reservedSuffixes {
		reserved[prefix+suffix] = true
	}

	out := map[string]string{}
	for k, v := range in {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if reserved[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// secretName is the ArgoCD cluster Secret name for a child.
func secretName(cluster string) string { return "cluster-" + cluster }

// checkOwnership decides whether c may write the cluster Secret that already
// exists. It returns a conflictError when it may not.
//
// This is the tool's security boundary, and until 0.4.0 there was not one: apply
// read the Secret, overwrote it and relabelled it as ours no matter who wrote it.
// A tenant able to label their own namespace `<prefix>cluster: prod` could
// therefore repoint ArgoCD's `prod` registration at their own API server with
// their own credentials, and -- because the takeover also rewrote
// `source-namespace` -- make it garbage-collectable on their own terms.
//
// The rule is incumbency. Whoever holds the registration keeps it, and a
// challenger is refused rather than arbitrated against, which is what Kubernetes
// itself does for name collisions (409 on create) and what every comparable
// controller does for a derived object: cert-manager, external-dns, External
// Secrets, Crossplane, and the ControllerRef rule that a controller may adopt an
// orphan but never something another controller already owns.
func (r *Registrar) checkOwnership(existing *coreV1.Secret, c child) error {
	if existing.Labels[r.managedByLabel()] != r.cfg.ManagedByValue {
		// Hand-registered, or another registrar's. Either way not ours to take.
		return conflictf(
			"secret %s/%s is not managed by this registrar (%s=%q); "+
				"refusing to take it over for namespace %s. Rename the cluster, or delete "+
				"the secret if it is genuinely stale",
			r.cfg.TargetNamespace, existing.Name, r.managedByLabel(),
			existing.Labels[r.managedByLabel()], c.namespace)
	}

	switch src := existing.Labels[r.sourceNamespaceLabel()]; src {
	case c.namespace:
		return nil

	case "":
		// No recorded owner. Adopt only if the Secret already names this exact
		// cluster, which is the ControllerRef "adopt an orphan" rule.
		//
		// This is deliberately narrow, because the population it was originally
		// written for turns out not to exist: v0.1.8 wrote an UNPREFIXED
		// managed-by, v0.2.0 wrote every label including source-namespace, and
		// the one commit in between was squashed into the v0.2.0 merge and is in
		// no release. So a genuine pre-0.2.0 Secret fails the managed-by check
		// above and never reaches here. What does reach here is a Secret whose
		// source-namespace label was stripped by someone with write access to the
		// ArgoCD namespace -- i.e. the takeover this function exists to prevent,
		// wearing a different hat. Requiring the cluster label to match means an
		// attacker gains nothing they did not already have.
		if existing.Labels[r.clusterLabel()] != c.cluster {
			return conflictf(
				"secret %s/%s is owned but records no source namespace, and its %s label is %q "+
					"rather than %q; refusing to adopt it for namespace %s",
				r.cfg.TargetNamespace, existing.Name, r.clusterLabel(),
				existing.Labels[r.clusterLabel()], c.cluster, c.namespace)
		}
		r.log.Warn("adopting an owned cluster secret that records no source namespace",
			slog.String("secret", existing.Name), slog.String("cluster", c.cluster),
			slog.String("namespace", c.namespace),
			slog.String("label", r.sourceNamespaceLabel()))
		return nil

	default:
		// Deliberately NOT compared by UID. A namespace deleted and recreated
		// under the same name is a legitimate claimant, and refusing it here
		// would deadlock: collect() would see a live namespace of that name and
		// refuse to delete the registration, so neither side could ever move.
		return conflictf(
			"cluster %q is registered from namespace %s; refusing to take it over for %s. "+
				"Two namespaces must not claim one cluster name",
			c.cluster, src, c.namespace)
	}
}

// claimContestedName decides whether c may CREATE the cluster Secret, when
// several managed namespaces claim the same cluster name and none holds it yet.
//
// Incumbency settles the case where a registration already exists; this settles
// the case where it does not. The oldest namespace wins, breaking an exact tie on
// name.
//
// Oldest-wins is the ecosystem's answer -- Gateway API, ingress-nginx and
// cert-manager all use creation timestamp then name -- and it is the right shape
// here for a specific reason: it is monotonic in the defender's favour. A
// namespace cannot become older, so an attacker who deletes and recreates one can
// only ever lose the tie. The alphabetical fallback is reachable only on an exact
// timestamp match and must never be relied on.
//
// Refusing BOTH claimants was considered and rejected. It is symmetric, so a
// standing claim on a guessable name would permanently block that cluster from
// ever registering, including after a legitimate rebuild -- trading a hijack risk
// for an availability one. Upstream reserves refuse-both for conflicts inside a
// single object with a single author, where it penalises only the author.
func (r *Registrar) claimContestedName(ctx context.Context, c child) error {
	selector := fmt.Sprintf("%s=%s,%s=%s",
		r.managedByLabel(), r.cfg.ManagedByValue, r.clusterLabel(), c.cluster)
	namespaces, err := r.client.CoreV1().Namespaces().List(ctx, metaV1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list claimants of cluster %q (%s): %w", c.cluster, selector, err)
	}

	winner := ""
	var winnerAt metaV1.Time
	for i := range namespaces.Items {
		ns := &namespaces.Items[i]
		// A namespace being torn down is not a claimant; discover skips these
		// too, and letting one win would block the survivor for as long as the
		// finalizers took.
		if ns.DeletionTimestamp != nil {
			continue
		}
		if winner == "" || ns.CreationTimestamp.Before(&winnerAt) ||
			(ns.CreationTimestamp.Equal(&winnerAt) && ns.Name < winner) {
			winner, winnerAt = ns.Name, ns.CreationTimestamp
		}
	}

	// Either we are the only claimant, or the list is somehow empty (our own
	// namespace was deleted mid-pass); both mean there is nobody to lose to.
	if winner == "" || winner == c.namespace {
		return nil
	}
	return conflictf(
		"cluster %q is also claimed by namespace %s, which is older; "+
			"not registering it from %s", c.cluster, winner, c.namespace)
}

func (r *Registrar) apply(ctx context.Context, c child) error {
	// Order matters: the propagated labels are copied LAST, so anything they can
	// reach wins over what was computed here. propagatedLabels already withholds
	// every reserved suffix for exactly that reason -- see reservedSuffixes.
	labels := map[string]string{
		argoSecretTypeLabel:         argoSecretTypeValue,
		r.managedByLabel():          r.cfg.ManagedByValue,
		r.clusterLabel():            c.cluster,
		r.sourceNamespaceLabel():    c.namespace,
		r.sourceNamespaceUIDLabel(): string(c.namespaceUID),
		r.providerLabel():           c.provider,
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
		// Nobody holds the name, so incumbency cannot settle it. Check for other
		// claimants BEFORE the dry-run return, or a pre-flight run reports two
		// namespaces each "would create" the same Secret and says nothing about
		// the conflict -- the one thing it would be run to find.
		if err := r.claimContestedName(ctx, c); err != nil {
			return err
		}
		if r.cfg.DryRun {
			r.log.Info("[dry-run] would create cluster secret",
				slog.String("cluster", c.cluster), slog.String("server", c.server))
			return nil
		}
		if _, err := api.Create(ctx, want, metaV1.CreateOptions{}); err != nil {
			if apiErrors.IsAlreadyExists(err) {
				// Someone wrote it between our Get and our Create. Benign, and
				// resolved by incumbency on the next pass -- which is also what
				// keeps this safe once more than one worker reconciles at a time.
				return conflictf("cluster secret %s was created concurrently; retrying next pass",
					want.Name)
			}
			return fmt.Errorf("create: %w", err)
		}
		r.log.Info("registered cluster",
			slog.String("cluster", c.cluster), slog.String("server", c.server))
		return nil

	case err != nil:
		return fmt.Errorf("get: %w", err)
	}

	// Before anything is written, and before the dry-run return below, so that a
	// pre-flight run reports a takeover it would otherwise have performed.
	if err := r.checkOwnership(existing, c); err != nil {
		return err
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
// of those as "deleted" would deregister live clusters.
//
// `applied` is what was actually written this pass, NOT everything discovered.
// A registration that was refused, or whose write failed, vouches for nothing.
//
// `unresolved` names the namespaces this pass could not establish a registration
// for -- unreadable at discovery, failed to apply, or refused. Their
// registrations are skipped outright, before the existence check, so a namespace
// that exists but could not be evaluated is never a deletion candidate at all.
// The namespace Get below is still the proof that matters -- this is the first
// line of defence, not a replacement for it.
func (r *Registrar) collect(ctx context.Context, applied []child, unresolved map[string]bool) error {
	live := map[string]bool{}
	for _, d := range applied {
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

		if unresolved[srcNS] {
			r.log.Debug("source namespace could not be evaluated this pass; not deleting",
				slog.String("secret", s.Name), slog.String("namespace", srcNS))
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
