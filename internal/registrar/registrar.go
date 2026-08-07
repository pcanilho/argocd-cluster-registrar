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

	// SuffixOrphanedSecretType parks the value of argoSecretTypeLabel on a
	// registration whose source has moved on. ArgoCD discovers clusters purely by
	// that label selector, so moving the key deregisters the cluster immediately
	// while destroying nothing -- a reversible delete.
	SuffixOrphanedSecretType = "orphaned-secret-type"

	// SuffixSupersededBy names the registration that replaced this one.
	SuffixSupersededBy = "superseded-by"

	// SuffixStaleSince records when the demotion happened, as a label-safe
	// timestamp: label values may not contain ':', so RFC3339 is not an option
	// and this uses the basic ISO 8601 form instead.
	SuffixStaleSince = "stale-since"

	// staleSinceFormat is that basic form, e.g. 20260807T143000Z.
	staleSinceFormat = "20060102T150405Z"

	// SuffixPrune opts a single registration out of removal. Set it to
	// PruneDisabled on the cluster Secret and neither deletion nor demotion will
	// touch it.
	//
	// It exists because reconciliation is event-driven: a mistaken label edit used
	// to take up to a full interval to become a deletion, which was often long
	// enough for a human to notice, and now it does not. Every comparable tool
	// ships an escape hatch for the same reason.
	SuffixPrune = "prune"

	// PruneDisabled is the only value SuffixPrune recognises. Anything else is
	// ignored, so a typo fails safe towards normal collection rather than
	// silently pinning a registration forever.
	PruneDisabled = "disabled"

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

// OrphanedSecretTypeLabel is the parked-secret-type label key for a given prefix.
func OrphanedSecretTypeLabel(prefix string) string { return prefix + SuffixOrphanedSecretType }

// SupersededByLabel is the superseded-by label key for a given prefix.
func SupersededByLabel(prefix string) string { return prefix + SuffixSupersededBy }

// StaleSinceLabel is the stale-since label key for a given prefix.
func StaleSinceLabel(prefix string) string { return prefix + SuffixStaleSince }

// PruneLabel is the prune opt-out label key for a given prefix.
func PruneLabel(prefix string) string { return prefix + SuffixPrune }

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
	SuffixOrphanedSecretType,
	SuffixSupersededBy,
	SuffixStaleSince,
	// Reserved so it cannot be propagated off a source namespace. The opt-out is
	// for whoever owns the ArgoCD namespace; letting a tenant set it on their own
	// namespace would let them pin a registration in place after their cluster
	// was gone, which is the immortal-registration problem this list exists for.
	SuffixPrune,
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
// All four have been run against the real thing; see the provisioner table in
// README.md, which distinguishes tested from assumed and should be kept honest.
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
	base, err := BaseRestConfig()
	if err != nil {
		return nil, err
	}
	client, err := ClientFor(base)
	if err != nil {
		return nil, err
	}
	return NewWithClient(log, cfg, client), nil
}

// NewWithClient builds a Registrar around a client the caller already has.
//
// The controller needs this: its manager owns the connection, and the registrar
// must read and write through a clientset built from the same base config but
// with a different timeout. See ClientFor.
func NewWithClient(log *slog.Logger, cfg Config, client kubernetes.Interface) *Registrar {
	if cfg.LabelPrefix == "" {
		cfg.LabelPrefix = DefaultLabelPrefix
	}
	return &Registrar{client: client, cfg: cfg, log: log}
}

// ClientFor builds the clientset used for every read and write, with the request
// timeout applied to a COPY of the config.
//
// The copy matters. rest.Config.Timeout becomes http.Client.Timeout, which the
// client appends to watch requests as ?timeout=30s, so a manager sharing this
// config would have every watch stream severed every thirty seconds and silently
// re-established. Give the manager the untouched base and keep the timeout here,
// where it protects exactly what it was added for.
func ClientFor(base *rest.Config) (kubernetes.Interface, error) {
	cfg := rest.CopyConfig(base)
	cfg.Timeout = clientTimeout
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	return client, nil
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

func (r *Registrar) orphanedSecretTypeLabel() string {
	return OrphanedSecretTypeLabel(r.cfg.LabelPrefix)
}
func (r *Registrar) pruneLabel() string        { return PruneLabel(r.cfg.LabelPrefix) }
func (r *Registrar) supersededByLabel() string { return SupersededByLabel(r.cfg.LabelPrefix) }
func (r *Registrar) staleSinceLabel() string   { return StaleSinceLabel(r.cfg.LabelPrefix) }

// clientTimeout bounds every ordinary API call. Without it a hung request inside
// a reconcile blocks forever and the pod sits Running and Ready while doing
// nothing at all; nothing else in the stack detects that, because the process has
// not crashed.
//
// It is applied to the clientset's own copy of the config and nowhere else. A
// watch must never carry it -- see ClientFor.
const clientTimeout = 30 * time.Second

// BaseRestConfig is the ambient connection with NO timeout applied. Callers that
// issue ordinary requests should go through ClientFor; anything that watches must
// use this untouched.
func BaseRestConfig() (*rest.Config, error) {
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

// ReconcileOne reconciles everything keyed on a single source namespace: the
// registration that namespace should have, and every registration recorded
// against it.
//
// This is the unit both drivers work in. The sweep below calls it once per key;
// the controller calls it per event. It returns only an error, never a requeue
// result, so that a driver cannot forget to schedule the next visit -- that
// decision belongs to the driver, in one place.
//
// The namespace read is the pass's single source of truth about existence, and it
// must not come from a cache. Against a label-filtered cache the apiserver emits a
// synthetic Deleted when an object stops matching the selector, so a cached
// NotFound cannot tell "the namespace was deleted" from "someone removed the
// managed-by label" -- and the second must never deregister anything.
// The bool reports that this key is DONE: the namespace is gone and it owns
// nothing further, so there is no reason to visit it again. Anything else is
// false, including the case where the namespace merely stopped being ours --
// that key must stay in the queue, because the filtered cache has already
// forgotten the namespace and will not report its eventual deletion.
func (r *Registrar) ReconcileOne(ctx context.Context, nsName string) (bool, error) {
	ns, err := r.client.CoreV1().Namespaces().Get(ctx, nsName, metaV1.GetOptions{})
	switch {
	case apiErrors.IsNotFound(err):
		// The proof. This is the only path that may delete.
		remaining, cErr := r.collectOne(ctx, nsName, false, "")
		return remaining == 0 && cErr == nil, cErr
	case err != nil:
		// Nothing is proven, so nothing may be collected.
		return false, fmt.Errorf("confirm namespace %s: %w", nsName, err)
	}

	// Still ours? Under a watch this is not redundant with discovery. A cache
	// filtered on the ownership label reports an object that stops matching as a
	// synthetic Delete, so removing the label enqueues the namespace exactly like
	// deleting it would -- and the namespace itself is still there, cluster label
	// and all. Without this check the next reconcile would happily re-register a
	// namespace that had just been taken out of our scope.
	//
	// Not a deletion either: nothing here proves the cluster is gone, so its
	// registrations are left exactly as they are.
	if ns.Labels[r.managedByLabel()] != r.cfg.ManagedByValue {
		r.log.Warn("namespace no longer carries the ownership label; leaving its registrations alone",
			slog.String("namespace", ns.Name), slog.String("label", r.managedByLabel()))
		return false, nil
	}

	// A namespace being torn down still reads back; registering from it would
	// re-create a Secret that collection is about to remove. Deliberately NOT
	// treated as unevaluable: see the applied == "" guard in collectOne, which
	// this is the sole caller reaching.
	if ns.DeletionTimestamp != nil {
		r.log.Debug("skipping terminating namespace", slog.String("namespace", ns.Name))
		_, err := r.collectOne(ctx, nsName, true, "")
		return false, err
	}

	c, ok := r.discoverOne(ctx, ns)
	if !ok {
		// Could not be evaluated. Skip collection entirely rather than concluding
		// its registrations are orphaned.
		return false, nil
	}

	if err := r.apply(ctx, c); err != nil {
		var conflict *conflictError
		if errors.As(err, &conflict) {
			// Not a failure, so it must not fail the pass: a contested name
			// persists until a human fixes it, and counting it would mark every
			// pass failed forever. Logged at Error all the same -- a silently
			// resolved conflict over a credential-bearing Secret is the hazard.
			//
			// Skip collection: a refused claimant vouches for nothing, and its own
			// earlier registrations must not be read as superseded.
			r.log.Error("refused to register cluster", slog.String("cluster", c.cluster),
				slog.String("namespace", c.namespace), slog.Any("reason", err))
			return false, nil
		}
		r.log.Error("failed to register cluster",
			slog.String("cluster", c.cluster), slog.Any("error", err))
		return false, fmt.Errorf("%s: %w", c.cluster, err)
	}

	_, err = r.collectOne(ctx, nsName, true, secretName(c.cluster))
	return false, err
}

// AuditUnrouted reports owned Secrets that record no source namespace.
//
// Nothing can collect them: every collection path selects on
// <prefix>source-namespace=<name>, and they match no value of it. Nor can any
// event route to them, since the key they would map to is empty. So they are
// invisible unless something goes looking, which is what this does.
//
// The population is not historical. No released version ever wrote the ownership
// label without also writing the source, so a Secret in this state got there by
// someone stripping the label off it by hand.
func (r *Registrar) AuditUnrouted(ctx context.Context) error {
	selector := fmt.Sprintf("%s=%s", r.managedByLabel(), r.cfg.ManagedByValue)
	secrets, err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).
		List(ctx, metaV1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list cluster secrets (%s): %w", selector, err)
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if s.Labels[r.sourceNamespaceLabel()] != "" {
			continue
		}
		r.log.Warn("owned cluster secret records no source namespace; it can never be collected",
			slog.String("secret", s.Name), slog.String("label", r.sourceNamespaceLabel()),
			slog.String("fix", "restore the label, or delete the secret if the cluster is gone"))
	}
	return nil
}

// reconcileKeys is every source namespace worth visiting: those currently
// labelled for us, plus those recorded on a registration we own.
//
// The second half is what lets a sweep collect anything at all. A namespace that
// has been deleted no longer lists, so without reading its name back off the
// Secret it left behind, nothing would ever revisit it.
func (r *Registrar) reconcileKeys(ctx context.Context) ([]string, error) {
	keys := map[string]bool{}

	nsSelector := fmt.Sprintf("%s=%s", r.managedByLabel(), r.cfg.ManagedByValue)
	namespaces, err := r.client.CoreV1().Namespaces().List(ctx, metaV1.ListOptions{LabelSelector: nsSelector})
	if err != nil {
		return nil, fmt.Errorf("list namespaces (%s): %w", nsSelector, err)
	}
	for i := range namespaces.Items {
		keys[namespaces.Items[i].Name] = true
	}

	secrets, err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).
		List(ctx, metaV1.ListOptions{LabelSelector: nsSelector})
	if err != nil {
		return nil, fmt.Errorf("list cluster secrets (%s): %w", nsSelector, err)
	}
	for i := range secrets.Items {
		if src := secrets.Items[i].Labels[r.sourceNamespaceLabel()]; src != "" {
			keys[src] = true
		}
	}

	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	// Stable ordering keeps logs diffable between passes. Nothing may depend on
	// it: which claimant wins a contested name is decided by claimContestedName,
	// on the namespace's creation timestamp, precisely so that it does not vary
	// with the order a driver happens to present namespaces in.
	sort.Strings(out)
	return out, nil
}

// Reconcile performs one full sweep over every key. It is what --once runs, and
// what the poll loop ran before the controller existed.
//
// Re-reading each kubeconfig is what keeps registrations valid across a k3s
// server restart -- those rotate the child's client certificate, and a stale
// Secret breaks every Application targeting that cluster with an authentication
// error rather than a visible one.
func (r *Registrar) Reconcile(ctx context.Context) error {
	keys, err := r.reconcileKeys(ctx)
	if err != nil {
		return err
	}

	var errs []string
	for _, k := range keys {
		// One bad namespace must not stop the others, or a single broken cluster
		// would stall the whole fleet.
		if _, err := r.ReconcileOne(ctx, k); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if err := r.AuditUnrouted(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("audit: %v", err))
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

// discoverOne evaluates a single managed namespace. The bool reports whether a
// registration could be established; false means "not this time", never "gone".
//
// That distinction is the whole safety story. Every false return below is a state
// a healthy cluster passes through -- a k3k child has no kubeconfig at all for its
// first ninety seconds, and a half-written one does not parse -- so treating any
// of them as a deletion would deregister live clusters. The caller must skip
// garbage collection entirely for a namespace that returns false, rather than
// concluding its registrations are orphaned.
func (r *Registrar) discoverOne(ctx context.Context, ns *coreV1.Namespace) (child, bool) {
	name := ns.Labels[r.clusterLabel()]
	if name == "" {
		r.log.Warn("namespace is managed but has no cluster label; skipping",
			slog.String("namespace", ns.Name), slog.String("label", r.clusterLabel()))
		return child{}, false
	}

	// The name becomes a Secret name, so it must be a valid DNS-1123 subdomain.
	// Label values legally allow uppercase, "_" and ".", none of which are, and
	// the apiserver would otherwise reject the write on every pass forever.
	if errs := validation.IsDNS1123Subdomain(secretName(name)); len(errs) > 0 {
		r.log.Error("cluster name does not yield a valid Secret name; skipping",
			slog.String("namespace", ns.Name), slog.String("cluster", name),
			slog.String("reason", strings.Join(errs, "; ")))
		return child{}, false
	}

	// Two namespaces may claim one cluster name. That is NOT resolved here:
	// discovery is per-namespace and cannot see the other claimant. apply()
	// settles it -- see claimContestedName.
	//
	// Note the name is not claimed until apply() succeeds. Claiming it here,
	// before the kubeconfig lookup below, used to mean a namespace that never
	// produced a usable kubeconfig still poisoned a healthy namespace claiming
	// the same name, and neither ever registered.
	candidates, err := r.findKubeconfigCandidates(ctx, ns.Name)
	if err != nil {
		var apiErr *apiFailure
		if errors.As(err, &apiErr) {
			// A genuine API failure, NOT "the provisioner has not written it
			// yet". Logging this as routine is how a fleet-wide deregistration
			// used to look reassuring in the logs.
			r.log.Error("could not read secrets in managed namespace",
				slog.String("namespace", ns.Name), slog.String("cluster", name), slog.Any("error", err))
			return child{}, false
		}
		// Entirely normal between the Cluster CR being created and its server
		// becoming ready -- k3k only writes the kubeconfig once the API is up,
		// and reports ProvisioningFailed for roughly the first ninety seconds
		// while it does.
		r.log.Info("no kubeconfig secret yet; will retry",
			slog.String("namespace", ns.Name), slog.String("cluster", name), slog.Any("reason", err))
		return child{}, false
	}

	// Try candidates in order. A shape match is not a usable kubeconfig: it may
	// be half-written, or carry an exec credential that cannot be copied into an
	// ArgoCD Secret, and in both cases the next candidate may be fine.
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
		return child{}, false
	}

	return child{
		cluster:      name,
		namespace:    ns.Name,
		namespaceUID: ns.UID,
		server:       server,
		config:       config,
		provider:     provider,
		labels:       propagatedLabels(ns.Labels, r.cfg.LabelPrefix),
	}, true
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

// demote takes a registration out of ArgoCD's sight without destroying it.
//
// Deleting would be simpler and is what a prune-style tool does, but this Secret
// is not solely ours: ArgoCD writes `namespaces`, `clusterResources` and
// `project` into it, and operators add annotations and foreign labels. apply()
// goes to some length not to clobber those (see the read-modify-write there), and
// deleting on a rename would throw away everything it protects. A mistaken rename
// that gets reverted would silently take an operator's per-cluster configuration
// with it.
//
// ArgoCD discovers clusters purely by the argocd.argoproj.io/secret-type label
// selector, so parking that one key deregisters the cluster immediately and
// completely, while every byte survives. It also self-heals: apply() copies
// want.Labels over the existing object, restoring secret-type, and its
// prefixed-label sweep removes the three labels written here because they are
// prefixed and absent from want.Labels. So reverting the rename restores the
// registration, credentials and all, with no special case anywhere.
func (r *Registrar) demote(ctx context.Context, s *coreV1.Secret, supersededBy string) error {
	if r.cfg.DryRun {
		r.log.Info("[dry-run] would demote superseded cluster secret",
			slog.String("secret", s.Name), slog.String("supersededBy", supersededBy))
		return nil
	}

	updated := s.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	if v, ok := updated.Labels[argoSecretTypeLabel]; ok {
		updated.Labels[r.orphanedSecretTypeLabel()] = v
		delete(updated.Labels, argoSecretTypeLabel)
	}
	// superseded-by is a breadcrumb, and it is derived from a cluster name that is
	// only bounded by the 63 bytes a LABEL VALUE allows -- so "cluster-" + name can
	// be 71, which the apiserver rejects. Dropping the breadcrumb is very much
	// better than failing the write: the load-bearing half of demotion is parking
	// the ArgoCD label above, and without it the stale registration stays visible
	// and ArgoCD ends up with two live clusters on one server URL.
	if errs := validation.IsValidLabelValue(supersededBy); len(errs) > 0 {
		r.log.Warn("superseded-by would not be a valid label value; demoting without it",
			slog.String("secret", s.Name), slog.String("supersededBy", supersededBy),
			slog.String("reason", strings.Join(errs, "; ")))
	} else {
		updated.Labels[r.supersededByLabel()] = supersededBy
	}
	updated.Labels[r.staleSinceLabel()] = time.Now().UTC().Format(staleSinceFormat)

	if _, err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).
		Update(ctx, updated, metaV1.UpdateOptions{}); err != nil {
		return fmt.Errorf("demote %s: %w", s.Name, err)
	}
	r.log.Warn("demoted superseded cluster registration; it is no longer visible to ArgoCD but has been kept",
		slog.String("secret", s.Name), slog.String("supersededBy", supersededBy),
		slog.String("namespace", s.Labels[r.sourceNamespaceLabel()]))
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

// collectOne reconciles the registrations recorded against ONE source namespace.
//
// `exists` is the caller's proof about that namespace, obtained from a single
// uncached read. `applied` is the Secret name this namespace successfully wrote
// this pass, or "" if it wrote none.
//
// Deletion requires POSITIVE proof. A Secret is removed only when `exists` is
// false, which the caller sets only on a definite NotFound. Absence from
// `applied` is never evidence: a namespace can fail to produce a registration
// because an API call failed, because a kubeconfig was half-written, or because
// a label was edited, and treating any of those as "deleted" would deregister
// live clusters. The caller must not call this at all for a namespace it could
// not evaluate.
//
// `applied` is at most one Secret because a namespace carries exactly one cluster
// label. The set can still hold several Secrets, though: a rename leaves the old
// registration behind, demoted, still recorded against this namespace. That is
// why the selector matches a set and not a single name -- handling only one would
// leak every superseded registration the moment its namespace was deleted.
// The int reports how many owned Secrets still record this namespace after the
// pass. Zero, together with a namespace that is provably gone, means there is
// nothing left to come back for.
func (r *Registrar) collectOne(ctx context.Context, nsName string, exists bool, applied string) (int, error) {
	// Selected on ownership and source ALONE. Selecting on secret-type as well
	// would hide already-demoted registrations, which still need collecting once
	// their namespace finally goes away. managed-by is the ownership marker; the
	// ArgoCD label is incidental to us.
	selector := fmt.Sprintf("%s=%s,%s=%s",
		r.managedByLabel(), r.cfg.ManagedByValue, r.sourceNamespaceLabel(), nsName)

	secrets, err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).
		List(ctx, metaV1.ListOptions{LabelSelector: selector})
	if err != nil {
		return 0, fmt.Errorf("list cluster secrets (%s): %w", selector, err)
	}

	remaining := len(secrets.Items)
	var errs []string
	for i := range secrets.Items {
		s := &secrets.Items[i]

		if s.Name == applied {
			continue
		}

		// Opted out. Checked before BOTH removal paths, so it covers demotion as
		// well as deletion -- demotion is reversible, but it still takes a cluster
		// out of ArgoCD's sight, which is exactly what someone pinning a
		// registration is asking not to happen.
		if s.Labels[r.pruneLabel()] == PruneDisabled {
			r.log.Debug("registration is opted out of collection; leaving it",
				slog.String("secret", s.Name), slog.String("namespace", nsName),
				slog.String("label", r.pruneLabel()))
			continue
		}

		if !exists {
			if err := r.deleteOrphan(ctx, s, nsName); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			remaining--
			continue
		}

		// The namespace is still there, so nothing here is a deletion.
		//
		// This guard is load-bearing and must not be simplified away. A namespace
		// that is TERMINATING is skipped by the caller without being marked
		// unevaluable, so it reaches here with applied == "". Demoting on that
		// would hide a live registration the instant its namespace began to
		// terminate, moments before it was going to be deleted properly -- and if
		// a finalizer then aborted the deletion, it would stay hidden.
		//
		// Outside that case, "the namespace was fully evaluated" implies
		// applied != "": every other way discoverOne can fail makes the caller
		// skip garbage collection for this namespace entirely.
		if applied == "" {
			r.log.Warn("source namespace still exists but produced no registration; not deleting",
				slog.String("secret", s.Name), slog.String("namespace", nsName))
			continue
		}

		if s.Labels[r.orphanedSecretTypeLabel()] != "" {
			// Already demoted on an earlier pass. Nothing to do, and nothing
			// worth repeating every reconcile.
			r.log.Debug("superseded registration is already demoted",
				slog.String("secret", s.Name), slog.String("supersededBy", applied))
			continue
		}

		// The namespace registered under a DIFFERENT name, so it has disclaimed
		// this one and nothing will ever reclaim it. Positive proof of a second
		// kind, not an absence.
		//
		// Left alone, the stale Secret is not inert: it is never rewritten, so its
		// kubeconfig freezes with a certificate good for about a year, and ArgoCD
		// resolves two registrations sharing one server URL by taking whichever
		// its informer index yields first.
		if err := r.demote(ctx, s, applied); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return remaining, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return remaining, nil
}

// deleteOrphan removes a registration whose source namespace is provably gone.
func (r *Registrar) deleteOrphan(ctx context.Context, s *coreV1.Secret, nsName string) error {
	if r.cfg.DryRun {
		r.log.Info("[dry-run] would delete orphaned cluster secret",
			slog.String("secret", s.Name), slog.String("namespace", nsName))
		return nil
	}
	// Preconditioned on the Secret's own UID. Under the poll loop the gap between
	// the existence proof and this call was microseconds and single-threaded; under
	// a watch it is event latency plus a requeue, so a delete decided in one
	// reconcile can otherwise land on a Secret a later one has already recreated --
	// deregistering a cluster that is genuinely live.
	err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).Delete(ctx, s.Name, metaV1.DeleteOptions{
		Preconditions: &metaV1.Preconditions{UID: &s.UID},
	})
	switch {
	case err == nil:
		r.log.Info("deregistered cluster (source namespace gone)",
			slog.String("secret", s.Name), slog.String("namespace", nsName))
		return nil
	case apiErrors.IsNotFound(err):
		return nil
	case apiErrors.IsConflict(err):
		// The Secret was replaced between the read and the delete, so it is no
		// longer the object the proof was about. Leave it; the next reconcile sees
		// the new one and decides again.
		r.log.Info("cluster secret changed identity before deletion; leaving it",
			slog.String("secret", s.Name), slog.String("namespace", nsName))
		return nil
	default:
		// Keep going. One Secret held up by a finalizer or an admission webhook
		// must not stall collection for the rest.
		return fmt.Errorf("delete %s: %w", s.Name, err)
	}
}
