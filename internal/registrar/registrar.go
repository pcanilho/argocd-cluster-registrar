package registrar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// and UIDs are not, so this tells "the same namespace" apart from "a new
	// namespace wearing the old one's name". Read by collectStaleUID, and used as
	// a delete precondition so a collection decided in one pass cannot land on a
	// registration a later pass recreated.
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

	// SuffixProject is reserved by NAME only, for a future feature that may write
	// ArgoCD's `project` key. Deliberately absent from reservedSuffixes: ArgoCD
	// reads it from the Secret's Data, never from labels, so a prefixed label of
	// this name is inert to ArgoCD and withholding it would only break an
	// ApplicationSet selecting on it. A feature that did write it would be writing
	// Data, where a label denylist has no reach anyway.
	SuffixProject = "project"

	// SuffixShard is reserved by name for the same reason, and on the same terms.
	SuffixShard = "shard"

	// SuffixPrune opts a single registration out of removal. Set it to
	// PruneDisabled on the cluster Secret and neither deletion nor demotion will
	// touch it. Reconciliation is event-driven, so a mistaken label edit becomes a
	// deletion immediately; this is the escape hatch.
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

	// argoDomain is every key ArgoCD reads off a cluster Secret: secret-type,
	// skip-reconcile, refresh and the rest. Validate refuses a label prefix that
	// could reach any of them.
	argoDomain = "argocd.argoproj.io/"
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

// reservedSuffixes are the labels this tool derives itself, never copied from
// the source namespace: a namespace that could set them would lie about its own
// registration. `source-namespace` is the dangerous one, being the pointer
// garbage collection follows to prove a source is gone.
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
// Pattern and keys travel together because the key is the reliable
// discriminator: vcluster's `vc-*` matches both `vc-<name>` (key `config`) and
// `vc-config-<name>` (key `config.yaml`), and which sorts first varies by name.
type Provider struct {
	// Name identifies the provider in logs and on the cluster Secret's provider
	// label. It is not matched against anything.
	Name string

	// SecretNamePattern is a glob matched against Secret names within a
	// discovered namespace, e.g. "k3k-*-kubeconfig". Matched by name because the
	// provisioner owns that Secret and it carries none of our labels; the
	// namespace is the nearest object we do own, so intent lives there.
	SecretNamePattern string

	// SecretKeys are the keys that may hold the kubeconfig; Kamaji writes
	// `admin.conf` and `admin.svc` side by side. Every key present becomes its own
	// candidate and the first that parses wins, so an unusable key falls through
	// rather than stranding the namespace. Duplicates are rejected by Validate.
	SecretKeys []string

	// SecretType the provisioner stamps on its kubeconfig Secret, where it sets a
	// distinctive one. A preference, not a filter: not every provisioner sets one.
	SecretType coreV1.SecretType

	// AllowExec permits translating an exec credential found in this shape. Set
	// only on shapes that are exec-bearing by construction, so the loose globs
	// cannot pick one up by accident.
	//
	// Half the gate. Config.ExecCredentials is the other half, and both must be
	// on: this says the SHAPE is exec-bearing, that says the ArgoCD deployment
	// actually holds the cloud identity such a registration would authenticate as.
	AllowExec bool
}

// provenance scores how much a Secret looks like its provisioner wrote it, so
// that a hand-placed decoy loses to the real one whatever the names sort like.
// Neither signal is unforgeable; a dangling ownerReference does at least get the
// forgery garbage-collected.
func provenance(s *coreV1.Secret, p Provider) int {
	score := 0
	if p.SecretType != "" && s.Type == p.SecretType {
		score += 2
	}
	for i := range s.OwnerReferences {
		if c := s.OwnerReferences[i].Controller; c != nil && *c {
			score++
			break
		}
	}
	return score
}

// presets are the provisioner shapes shipped with the tool. See the provisioner
// table in README.md, which distinguishes tested from assumed.
//
// Every entry carries `#nosec G101`: gosec matches on `Secret*` identifiers next
// to a string literal, but these are name globs and data keys, not credentials.
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
	// Both keys are normally present and both parse, so `admin.conf` wins on
	// order. Driving Kamaji through its CAPI control-plane provider also produces
	// a second, CAPI-shaped Secret for the same cluster.
	"kamaji": { // #nosec G101 -- see above
		Name:              "kamaji",
		SecretNamePattern: "*-admin-kubeconfig",
		SecretKeys:        []string{"admin.conf", "admin.svc"},
	},
	// CAPA's second Secret for a managed EKS cluster, exec-only by construction,
	// which is why this and not `capi` may translate. Declare it BEFORE `capi` in
	// --provider order: both satisfy `value`, and `<c>-kubeconfig` sorts first, so
	// otherwise the 15-minute token wins.
	"capa-eks": { // #nosec G101 -- see above
		Name:              "capa-eks",
		SecretNamePattern: "*-user-kubeconfig",
		SecretKeys:        []string{"value"},
		AllowExec:         true,
	},
	// CAPZ's AAD user kubeconfig. Note the suffix order: CAPZ appends "-user" to
	// "<cluster>-kubeconfig", so this is not the CAPA spelling and `*-user-kubeconfig`
	// does not match it.
	"capz-aks": { // #nosec G101 -- see above
		Name:              "capz-aks",
		SecretNamePattern: "*-kubeconfig-user",
		SecretKeys:        []string{"value"},
		AllowExec:         true,
	},
	// The mandatory Cluster API control-plane contract, so one entry covers every
	// CAPI cluster whatever the infrastructure provider, plus standalone
	// k0smotron. The loosest pattern shipped; correctness comes from the key.
	"capi": { // #nosec G101 -- see above
		Name:              "capi",
		SecretNamePattern: "*-kubeconfig",
		SecretKeys:        []string{"value"},
		// Mandated by the same contract as the name and key.
		SecretType: "cluster.x-k8s.io/secret",
	},
}

// LeaderElectionID derives the lease name from the two values that decide whether
// two instances collide: label-prefix and managed-by, not the release name.
// Hashed because a label prefix contains "/", which an object name may not.
//
// _helpers.tpl computes the same thing and the two must agree, or a
// manifest-deployed instance and a chart-deployed one hold different leases and
// both reconcile. TestLeaderElectionIDMatchesTheChart pins them together.
func LeaderElectionID(labelPrefix, managedBy string) string {
	sum := sha256.Sum256([]byte(labelPrefix + "|" + managedBy))
	return "acr-" + hex.EncodeToString(sum[:])[:16]
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
// custom shape as "name=pattern=key[,key...][=exec]".
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
	if len(parts) != 3 && len(parts) != 4 {
		return Provider{}, fmt.Errorf(
			"malformed provider spec %q: expected \"name=pattern=key[,key...][=exec]\"", spec)
	}

	// The optional 4th field is the per-provider half of the exec gate, so a
	// custom shape can reach it too. Spelled out rather than a bare boolean,
	// because "true" would not say what it enables.
	allowExec := false
	if len(parts) == 4 {
		if strings.TrimSpace(parts[3]) != "exec" {
			return Provider{}, fmt.Errorf(
				"malformed provider spec %q: the 4th field must be exactly \"exec\", got %q",
				spec, parts[3])
		}
		allowExec = true
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
		AllowExec:         allowExec,
	}, nil
}

// Config controls a single reconcile pass.
type Config struct {
	// TargetNamespace is where ArgoCD reads cluster Secrets from. ArgoCD only
	// ever looks in its own namespace, so this is effectively always "argocd".
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

	// ExecCredentials permits translating exec credentials into ArgoCD's
	// argocd-k8s-auth configuration, for providers that also set AllowExec.
	//
	// Off by default because it changes who ArgoCD is to the clusters registered
	// this way: they authenticate with ArgoCD's own cloud identity rather than
	// with a credential from the source namespace.
	ExecCredentials bool

	// DemotedTTL is how long a demoted registration is kept before it is deleted
	// outright. Zero, the default, keeps them indefinitely.
	//
	// Bounds how far rename debris accumulates, at the price of bounding how long
	// a mistaken rename can still be reverted. Hence opt-in.
	DemotedTTL time.Duration
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
// timeout applied to a COPY of the config. rest.Config.Timeout becomes
// http.Client.Timeout, which the client appends to watches as ?timeout=30s, so a
// manager sharing this config would have every stream severed every 30s.
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
		seenKey := make(map[string]bool, len(p.SecretKeys))
		for _, k := range p.SecretKeys {
			if k == "" {
				return fmt.Errorf("provider %q: secret key must not be empty", p.Name)
			}
			// Rejected rather than quietly ignored, matching the duplicate-name rule
			// above. Every present key now becomes its own candidate, so a repeated
			// key would parse the same bytes twice and report the same failure twice
			// -- and the operator would never learn their values file has a typo.
			if seenKey[k] {
				return fmt.Errorf("provider %q: duplicate secret key %q", p.Name, k)
			}
			seenKey[k] = true
		}
		// A malformed glob is a permanent fault. It used to surface once per
		// namespace per pass, disguised as "no kubeconfig secret yet".
		if _, err := path.Match(p.SecretNamePattern, "probe"); err != nil {
			return fmt.Errorf("provider %q: secret name pattern %q is not a valid glob: %w",
				p.Name, p.SecretNamePattern, err)
		}
	}
	// A prefix ArgoCD's own keys fall under would let a source namespace propagate
	// one: they are not reserved suffixes, and propagated keys are copied last.
	// Checked against the domain rather than `secret-type` so it does not depend
	// on which key happens to be shortest, now that annotations reach
	// `skip-reconcile` and `refresh` too.
	//
	// Only this direction is reachable. A prefix BELOW the domain, such as
	// `argocd.argoproj.io/skip-`, would need to end in "/" to get this far and
	// would then yield a two-slash key that IsQualifiedName rejects below.
	if c.LabelPrefix != "" && strings.HasPrefix(argoDomain, c.LabelPrefix) {
		return fmt.Errorf("--label-prefix %q would let a source namespace override %s keys",
			c.LabelPrefix, argoDomain)
	}
	// Negative is a typo, not "disabled", and would expire everything on sight.
	if c.DemotedTTL < 0 {
		return fmt.Errorf("--demoted-ttl must not be negative, got %s", c.DemotedTTL)
	}
	// Empty is not "use the default" here. With no prefix, propagatedLabels
	// matches EVERY label on the source namespace and the reserved list computes
	// as bare names that match nothing actually written, so a tenant could
	// propagate argocd.argoproj.io/secret-type off its own namespace. The
	// constructor happens to repair it, but a caller that validates without going
	// through the constructor would get a silently different trust boundary.
	if c.LabelPrefix == "" {
		return fmt.Errorf("--label-prefix must not be empty; it is what keeps a source " +
			"namespace from propagating labels this tool reserves")
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

// clientTimeout bounds every ordinary API call: a hung request would otherwise
// leave the pod Running and Ready while doing nothing. Applied to the clientset's
// own copy of the config and nowhere else, since a watch must never carry it.
const clientTimeout = 30 * time.Second

// BaseRestConfig is the ambient connection with no timeout applied. Callers that
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
// registration it should have, and every registration recorded against it. The
// unit both the sweep and the controller work in.
//
// The namespace read is the single source of truth about existence and must not
// come from a cache: against a label-filtered cache the apiserver emits a
// synthetic Deleted when an object stops matching, so a cached NotFound cannot
// tell deletion from a removed managed-by label.
//
// The bool reports the key is done -- namespace gone, owning nothing further.
// A namespace that merely stopped being ours is false, because the filtered
// cache has forgotten it and will not report its eventual deletion.
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

	// Still ours? Removing the label enqueues the namespace exactly like deleting
	// it would, and the namespace is still there, cluster label and all. Not a
	// deletion: nothing here proves the cluster is gone.
	if ns.Labels[r.managedByLabel()] != r.cfg.ManagedByValue {
		r.log.Warn("namespace no longer carries the ownership label; leaving its registrations alone",
			slog.String("namespace", ns.Name), slog.String("label", r.managedByLabel()))
		return false, nil
	}

	// A namespace being torn down still reads back; registering from it would
	// re-create a Secret that collection is about to remove. Deliberately not
	// treated as unevaluable: see the applied == "" guard in collectOne, which
	// this is the sole caller reaching.
	if ns.DeletionTimestamp != nil {
		r.log.Debug("skipping terminating namespace", slog.String("namespace", ns.Name))
		_, err := r.collectOne(ctx, nsName, true, "")
		return false, err
	}

	// Before discovery, because the early return below is exactly the path that
	// would otherwise strand a predecessor's registration forever.
	if err := r.collectStaleUID(ctx, nsName, ns.UID); err != nil {
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
			// Not a failure: a contested name persists until a human fixes it, and
			// counting it would fail every pass forever. Still logged at Error,
			// because a silently resolved conflict over a credential-bearing Secret
			// is the hazard. Collection is skipped -- a refused claimant vouches for
			// nothing. The counter lives here because every refusal passes through.
			conflictsTotal.WithLabelValues(conflict.reason).Inc()
			r.log.Error("refused to register cluster", slog.String("cluster", c.cluster),
				slog.String("namespace", c.namespace),
				slog.String("conflict", conflict.reason), slog.Any("reason", err))
			return false, nil
		}
		r.log.Error("failed to register cluster",
			slog.String("cluster", c.cluster), slog.Any("error", err))
		return false, fmt.Errorf("%s: %w", c.cluster, err)
	}

	_, err = r.collectOne(ctx, nsName, true, secretName(c.cluster))
	return false, err
}

// AuditUnrouted reports owned Secrets that record no source namespace. Nothing
// can collect them -- every collection path selects on
// <prefix>source-namespace=<name> -- and no event routes to them, since their key
// would be empty. A Secret gets into this state by having the label stripped.
func (r *Registrar) AuditUnrouted(ctx context.Context) error {
	selector := fmt.Sprintf("%s=%s", r.managedByLabel(), r.cfg.ManagedByValue)
	secrets, err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).
		List(ctx, metaV1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list cluster secrets (%s): %w", selector, err)
	}
	// Counted while we are here: this LIST is the only place anything sees the
	// whole owned population at once, so the gauges are free. It is also the only
	// place that sees what ArgoCD is actually holding, which is what the expiry
	// buckets need -- the source kubeconfig may have moved on.
	//
	// Recomputed from the full population every pass, so it self-clears; the
	// per-conflict gauge PLAN.md rules out could not.
	counts := map[[2]string]int{}
	var unrouted int
	now := time.Now()

	for i := range secrets.Items {
		s := &secrets.Items[i]

		state := stateActive
		if s.Labels[r.orphanedSecretTypeLabel()] != "" {
			state = stateDemoted
		}
		bucket, notAfter, dated := credentialExpiry(s.Data["config"], now)
		counts[[2]string{state, bucket}]++

		// The cluster name goes in the LOG, never in a label: the no-tenant-names
		// rule is about cardinality on the metric, not about what may be said.
		// NotAfter only -- not Subject or SANs, which would disclose the privilege
		// level of a credential sitting in the ArgoCD namespace.
		if dated && (bucket == expiryExpired || bucket == expiryLt24h) {
			r.log.Warn("registration credential is expiring",
				slog.String("secret", s.Name),
				slog.String("cluster", s.Labels[r.clusterLabel()]),
				slog.String("state", state), slog.String("expiry", bucket),
				slog.Time("notAfter", notAfter))
		}
		// Only the broken case warns; absent is what an unwritten Secret looks like.
		if bucket == expiryUnreadable {
			r.log.Warn("registration credential could not be read",
				slog.String("secret", s.Name),
				slog.String("cluster", s.Labels[r.clusterLabel()]))
		}

		if s.Labels[r.sourceNamespaceLabel()] != "" {
			continue
		}
		unrouted++
		r.log.Warn("owned cluster secret records no source namespace; it can never be collected",
			slog.String("secret", s.Name), slog.String("label", r.sourceNamespaceLabel()),
			slog.String("fix", "restore the label, or delete the secret if the cluster is gone"))
	}

	// Every series set every pass, including back to zero, or a bucket that
	// emptied would keep reporting its last value forever.
	for _, state := range []string{stateActive, stateDemoted} {
		for _, bucket := range expiryBuckets {
			registrations.WithLabelValues(state, bucket).
				Set(float64(counts[[2]string{state, bucket}]))
		}
	}
	unroutedSecrets.Set(float64(unrouted))
	return nil
}

// reconcileKeys is every source namespace worth visiting: those currently
// labelled for us, plus those recorded on a registration we own. The second half
// is what lets a sweep collect at all, since a deleted namespace no longer lists.
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
	// Diffable logs only. Nothing may depend on it: claimContestedName decides a
	// contested name on creation timestamp, not on presentation order.
	sort.Strings(out)
	return out, nil
}

// Reconcile performs one full sweep over every key, which is what --once runs.
// Re-reading each kubeconfig keeps registrations valid across a k3s server
// restart: those rotate the child's client certificate, and a stale Secret breaks
// every Application targeting that cluster with an authentication error.
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
	annotations  map[string]string
}

// apiFailure marks an error as "the API call failed", as opposed to "the answer
// is legitimately not there yet". Only the former makes the view incomplete.
type apiFailure struct{ err error }

func (e *apiFailure) Error() string { return e.err.Error() }
func (e *apiFailure) Unwrap() error { return e.err }

// conflictError means the name is spoken for: nothing went wrong, the answer is
// "not yours", so Reconcile must not count it toward the pass's error return.
//
// The reason travels on the error rather than being re-derived from its text,
// because it is the metric's only label and rewording a sentence would otherwise
// be a silent monitoring regression.
type conflictError struct {
	reason string
	err    error
}

func (e *conflictError) Error() string { return e.err.Error() }
func (e *conflictError) Unwrap() error { return e.err }

// The closed set of reasons a claim can be refused. Constants because these are
// metric label values, and anything derived from a namespace or cluster name
// would be attacker-chosen cardinality on an explicitly adversarial path.
const (
	// conflictNotManaged is a Secret this registrar does not own at all: hand
	// written, or another instance's.
	conflictNotManaged = "not_managed"
	// conflictOrphanClusterMismatch is an owned Secret recording no source
	// namespace whose cluster label names a different cluster, so adopting it
	// would repoint someone else's registration.
	conflictOrphanClusterMismatch = "orphan_cluster_mismatch"
	// conflictIncumbent is the ordinary case: the name is held by another live
	// namespace and the holder keeps it.
	conflictIncumbent = "incumbent"
	// conflictContestedName is an unclaimed name several namespaces want, awarded
	// to the oldest.
	conflictContestedName = "contested_name"
	// conflictCreateRace is benign and self-resolving: two workers created the
	// same Secret at once. Expected during a leader-election handover, so it is
	// counted separately rather than alerted on.
	conflictCreateRace = "create_race"
	// conflictServerCollision is a second namespace claiming an address ArgoCD
	// already has a live registration for.
	conflictServerCollision = "server_collision"
)

func conflictf(reason, format string, a ...any) error {
	return &conflictError{reason: reason, err: fmt.Errorf(format, a...)}
}

// discoverOne evaluates a single managed namespace. The bool means "a
// registration could be established", never "gone" -- every false return is a
// state a healthy cluster passes through, so the caller must skip garbage
// collection entirely rather than conclude anything is orphaned.
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

	// A contested cluster name is not resolved here: discovery is per-namespace
	// and cannot see the other claimant. apply() settles it, and not before it
	// succeeds, so a namespace with no usable kubeconfig cannot poison a healthy
	// one claiming the same name.
	candidates, err := r.findKubeconfigCandidates(ctx, ns.Name)
	if err != nil {
		var apiErr *apiFailure
		if errors.As(err, &apiErr) {
			// A genuine API failure, not "the provisioner has not written it
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
	// be half-written, or carry an exec credential this provider may not
	// translate, and in both cases the next candidate may be fine.
	//
	// Two candidates may be the same Secret under different keys, which is why the
	// parse errors below are labelled `name[key]` rather than by name alone.
	var (
		server, config string
		provider       string
		parseErrs      []string
	)
	for _, c := range candidates {
		pk, perr := parseKubeconfig(c.secret.Data[c.key], r.cfg.ExecCredentials && c.allowExec)
		if perr != nil {
			parseErrs = append(parseErrs, fmt.Sprintf("%s[%s]: %v", c.secret.Name, c.key, perr))
			continue
		}
		server, config, provider = pk.server, pk.config, c.provider
		if len(parseErrs) > 0 {
			r.log.Debug("skipped unusable kubeconfig candidates before this one",
				slog.String("namespace", ns.Name), slog.String("using", c.secret.Name),
				slog.Any("skipped", parseErrs))
		}
		if c.provenance == 0 {
			r.log.Debug("registering from a secret with no provisioner provenance; "+
				"anyone who can write secrets in this namespace can supply it",
				slog.String("namespace", ns.Name), slog.String("cluster", name),
				slog.String("secret", c.secret.Name))
		}
		if pk.insecure {
			r.log.Debug("registration disables TLS verification; ArgoCD will not check "+
				"this cluster's certificate",
				slog.String("namespace", ns.Name), slog.String("cluster", name),
				slog.String("secret", c.secret.Name))
		}
		if pk.execCommand != "" {
			r.log.Info("registered with a translated exec credential; ArgoCD will "+
				"authenticate with its own cloud identity, not with a credential from "+
				"this namespace",
				slog.String("namespace", ns.Name), slog.String("cluster", name),
				slog.String("secret", c.secret.Name), slog.String("command", pk.execCommand))
		}
		break
	}
	if provider == "" {
		// Could be half-written, so this must not look like a deletion.
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
		annotations:  r.propagatedAnnotations(ns.Annotations, ns.Name),
	}, true
}

// candidate is a Secret that matched some provider, with that provider and the
// key holding the kubeconfig.
type candidate struct {
	secret   *coreV1.Secret
	provider string
	key      string
	// Carried so the caller can warn when the candidate it used had nothing
	// vouching for it.
	provenance int
	allowExec  bool
}

// findKubeconfigCandidates returns every (Secret, key) pair in ns where the
// Secret matches a configured provider's glob and carries that key, ordered by
// provider precedence, then provenance, then Secret name, then declared key order.
//
// Requiring the key as well as the name is what skips a decoy: vcluster's `vc-*`
// matches both `vc-<name>` (key `config`) and `vc-config-<name>` (key
// `config.yaml`).
//
// A list rather than one answer, because matching the shape is not the same as
// being usable. CAPA's EKS path writes a second `<cluster>-user-kubeconfig`
// under the same glob and key, and Kamaji writes `admin.conf` and `admin.svc`
// side by side. A parse failure is how an unusable candidate announces itself,
// and the caller falls through to the next.
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
		// Most provisioner-written first. Sorted per provider because the expected
		// Secret type is; stable over name-sorted input, so ties keep name order.
		matched := make([]int, 0, len(idx))
		for _, i := range idx {
			ok, err := path.Match(p.SecretNamePattern, secrets.Items[i].Name)
			if err != nil {
				// A permanent configuration fault, not a transient one. Validate()
				// checks every pattern up front so this should be unreachable.
				return nil, &apiFailure{fmt.Errorf("provider %q: bad secret name pattern %q: %w",
					p.Name, p.SecretNamePattern, err)}
			}
			if ok {
				matched = append(matched, i)
			}
		}
		sort.SliceStable(matched, func(a, b int) bool {
			return provenance(&secrets.Items[matched[a]], p) >
				provenance(&secrets.Items[matched[b]], p)
		})

		for _, i := range matched {
			s := &secrets.Items[i]
			// The patterns overlap by design, and a duplicate entry would only
			// produce the same registration.
			if claimed[s.Name] {
				continue
			}

			// One candidate per key PRESENT, so a half-written first key does not
			// put the second out of reach. Several candidates may share one Secret
			// pointer; read-only, and the caller tells them apart by key.
			found := 0
			for _, k := range p.SecretKeys {
				if _, has := s.Data[k]; !has {
					continue
				}
				found++
				out = append(out, candidate{
					secret: s, provider: p.Name, key: k,
					provenance: provenance(s, p), allowExec: p.AllowExec,
				})
			}
			if found == 0 {
				namedButKeyless = append(namedButKeyless,
					fmt.Sprintf("%s (provider %s)", s.Name, p.Name))
				continue
			}

			// Per SECRET, not per key: a later provider must not re-offer this
			// object under a key of its own, or one cluster registers twice.
			claimed[s.Name] = true
		}
	}

	if len(out) > 0 {
		return out, nil
	}
	// Only when nothing matched at all: with overlapping patterns a keyless
	// near-miss is routine and reporting it on a successful pass is noise.
	if len(namedButKeyless) > 0 {
		return nil, fmt.Errorf("secret(s) %v matched a provider pattern but carried none of its keys",
			namedButKeyless)
	}
	return nil, fmt.Errorf("no secret matching any configured provider")
}

// propagatedLabels copies the prefixed labels the ApplicationSet selects on,
// minus the ones this tool derives for itself. The exclusions are a trust
// boundary: everything here comes off a tenant-controllable namespace, and
// apply() copies it over the labels it just computed, so anything not excluded
// can be spoofed.
func propagatedLabels(in map[string]string, prefix string) map[string]string {
	out, _ := propagate(in, prefix, 0)
	return out
}

// propagatedAnnotations is the same for annotations, which the ApplicationSet
// cluster generator exposes as {{metadata.annotations.<key>}} exactly as it does
// labels. Worth having because an annotation has neither the 63-byte cap nor the
// charset a label value is held to, so a URL or a list can reach a template.
//
// Bounded, unlike labels, which the apiserver bounds for us. Annotations allow
// 256KB per object and every registration is held in ArgoCD's cluster informer,
// so without a cap the source namespace picks ArgoCD's memory footprint.
func (r *Registrar) propagatedAnnotations(in map[string]string, ns string) map[string]string {
	out, dropped := propagate(in, r.cfg.LabelPrefix, maxPropagatedAnnotationBytes)
	for _, k := range dropped {
		r.log.Warn("annotation is too large to propagate; skipping it",
			slog.String("namespace", ns), slog.String("annotation", k),
			slog.Int("limit", maxPropagatedAnnotationBytes))
	}
	return out
}

// maxPropagatedAnnotationBytes bounds one propagated annotation value.
const maxPropagatedAnnotationBytes = 4096

// propagate copies prefixed keys minus the reserved ones. A limit of 0 means
// unbounded; anything over it is returned in `dropped` rather than truncated,
// since a truncated URL is a working-looking wrong answer.
func propagate(in map[string]string, prefix string, limit int) (out map[string]string, dropped []string) {
	reserved := map[string]bool{}
	for _, suffix := range reservedSuffixes {
		reserved[prefix+suffix] = true
	}

	out = map[string]string{}
	for k, v := range in {
		if !strings.HasPrefix(k, prefix) || reserved[k] {
			continue
		}
		if limit > 0 && len(v) > limit {
			dropped = append(dropped, k)
			continue
		}
		out[k] = v
	}
	sort.Strings(dropped)
	return out, dropped
}

// secretName is the ArgoCD cluster Secret name for a child.
func secretName(cluster string) string { return "cluster-" + cluster }

// checkOwnership decides whether c may write the cluster Secret that already
// exists, returning a conflictError when it may not. This is the tool's security
// boundary: without it, anyone who can label a namespace `<prefix>cluster: prod`
// could repoint ArgoCD's `prod` registration at their own API server.
//
// The rule is incumbency. Whoever holds the registration keeps it and a
// challenger is refused, matching the ControllerRef rule that a controller may
// adopt an orphan but never something another controller owns.
func (r *Registrar) checkOwnership(existing *coreV1.Secret, c child) error {
	if existing.Labels[r.managedByLabel()] != r.cfg.ManagedByValue {
		// Hand-registered, or another registrar's. Either way not ours to take.
		return conflictf(conflictNotManaged,
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
		// cluster. Narrow on purpose: what reaches here is a Secret whose
		// source-namespace label was stripped by hand, which is the takeover this
		// function exists to prevent, so requiring the cluster label to match means
		// an attacker gains nothing they did not already have.
		if existing.Labels[r.clusterLabel()] != c.cluster {
			return conflictf(conflictOrphanClusterMismatch,
				"secret %s/%s is owned but records no source namespace, and its %s label is %q "+
					"rather than %q; refusing to adopt it for namespace %s",
				r.cfg.TargetNamespace, existing.Name, r.clusterLabel(),
				existing.Labels[r.clusterLabel()], c.cluster, c.namespace)
		}
		// Counted here, not at the refusal choke point: this branch is a success.
		adoptionsTotal.Inc()
		r.log.Warn("adopting an owned cluster secret that records no source namespace",
			slog.String("secret", existing.Name), slog.String("cluster", c.cluster),
			slog.String("namespace", c.namespace),
			slog.String("label", r.sourceNamespaceLabel()))
		return nil

	default:
		// not compared by UID: a namespace deleted and recreated under the same
		// name is a legitimate claimant, and refusing it here would deadlock
		// against collection, which sees a live namespace and keeps the Secret.
		return conflictf(conflictIncumbent,
			"cluster %q is registered from namespace %s; refusing to take it over for %s. "+
				"Two namespaces must not claim one cluster name",
			c.cluster, src, c.namespace)
	}
}

// claimContestedName decides whether c may CREATE the cluster Secret when
// several managed namespaces claim the same name and none holds it yet.
// Incumbency settles the case where a registration exists; this settles the case
// where it does not. Oldest namespace wins, ties broken on name.
//
// Oldest-wins because it is monotonic in the defender's favour: a namespace
// cannot become older, so an attacker who recreates one can only lose the tie.
// Refusing both would be symmetric, letting a standing claim on a guessable name
// block that cluster forever.
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
		// A terminating namespace is not a claimant; letting one win would block
		// the survivor for as long as its finalizers took.
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
	return conflictf(conflictContestedName,
		"cluster %q is also claimed by namespace %s, which is older; "+
			"not registering it from %s", c.cluster, winner, c.namespace)
}

// claimServer refuses a registration whose server URL is already held by a live
// registration from a different source namespace.
//
// ArgoCD keys clusters by address: its indexer and GetClusterByURL both trim a
// trailing slash, and the lookup returns items[0] from a set, so two Secrets on
// one address are two claims on one identity resolved arbitrarily. Upstream
// treats that as unsupported -- its own API refuses it, deriving the Secret name
// from the URL -- and it splits sharding, cycles the UI, and makes one RBAC
// decision cover both.
//
// Selected on ArgoCD's own label, not on ours. A hand-written cluster Secret or
// one from a second registrar instance is ambiguous to ArgoCD just the same, and
// this namespace is already listed elsewhere so the wider selector is free.
func (r *Registrar) claimServer(ctx context.Context, c child) error {
	selector := fmt.Sprintf("%s=%s", argoSecretTypeLabel, argoSecretTypeValue)
	secrets, err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).
		List(ctx, metaV1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list cluster secrets (%s): %w", selector, err)
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		src := s.Labels[r.sourceNamespaceLabel()]
		if src != "" && src == c.namespace {
			continue
		}
		if s.Labels[r.orphanedSecretTypeLabel()] != "" {
			continue
		}
		if strings.TrimRight(string(s.Data["server"]), "/") != c.server {
			continue
		}
		pinned := s.Labels[r.pruneLabel()] == PruneDisabled

		// An unpinned incumbent whose source namespace is gone is an orphan that
		// collection will remove anyway; releasing the address now just avoids
		// refusing a rebuild for one pass. A pinned one is deliberate, so it keeps
		// its address -- the operator said keep it, and unpinning is how they say
		// otherwise.
		if !pinned && src != "" && !r.namespaceExists(ctx, src) {
			r.log.Info("incumbent registration's source namespace is gone; releasing its address",
				slog.String("secret", s.Name), slog.String("namespace", src),
				slog.String("server", c.server))
			continue
		}

		held := src
		if held == "" {
			held = "outside this registrar"
		}
		hint := "To scope one cluster several ways, use a single registration with " +
			"ArgoCD's `namespaces` key, or AppProject destinationServiceAccounts"
		if pinned {
			hint = fmt.Sprintf("The incumbent is pinned with %s=%s; remove that label to "+
				"let it be replaced", r.pruneLabel(), PruneDisabled)
		}
		return conflictf(conflictServerCollision,
			"server %s is already registered as %s (from %s); refusing to register it again "+
				"as %s from %s, because ArgoCD identifies a cluster by its address. %s",
			c.server, s.Name, held, c.cluster, c.namespace, hint)
	}
	return nil
}

// namespaceExists answers the one question claimServer needs about an incumbent.
// Any uncertainty answers "yes", so an API blip never releases a held address.
func (r *Registrar) namespaceExists(ctx context.Context, name string) bool {
	_, err := r.client.CoreV1().Namespaces().Get(ctx, name, metaV1.GetOptions{})
	return !apiErrors.IsNotFound(err)
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
			Name:        secretName(c.cluster),
			Namespace:   r.cfg.TargetNamespace,
			Labels:      labels,
			Annotations: c.annotations,
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
		if err := r.claimServer(ctx, c); err != nil {
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
				return conflictf(conflictCreateRace,
					"cluster secret %s was created concurrently; retrying next pass",
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
	// Only when the address actually moves. Checking every update would cost a LIST
	// of the ArgoCD namespace on every reconcile, for a value that rarely changes.
	if was := string(existing.Data["server"]); was != c.server {
		if err := r.claimServer(ctx, c); err != nil {
			return err
		}
		// Updated in place, with the warning as the whole remedy. ArgoCD only
		// invalidates a cache entry it can still find by the NEW address
		// (argo-cd#14410), so it keeps watching the old one until its application
		// controller restarts. Delete-and-recreate would clear that but is not
		// atomic and would drop ArgoCD's own keys: leaked watches recover with a
		// restart, a destroyed registration does not.
		r.log.Warn("cluster address changed; ArgoCD may keep watching the old one "+
			"until its application controller restarts",
			slog.String("cluster", c.cluster), slog.String("from", was),
			slog.String("to", c.server))
	}
	if r.cfg.DryRun {
		r.log.Info("[dry-run] would update cluster secret", slog.String("cluster", c.cluster))
		return nil
	}

	// Read-modify-write, not a wholesale replace: Update replaces the whole
	// object, so a freshly-built Secret would drop ArgoCD's own `namespaces`,
	// `clusterResources` and `project` keys plus any operator annotations, and
	// changed() cannot see that drift either.
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
	// Drop prefixed labels absent upstream, so a cluster can be opted back out of
	// a selector. Except the prune opt-out, which is set here and has no upstream
	// to be absent from. The demotion labels are still swept: that is the
	// self-heal on a reverted rename.
	for k := range updated.Labels {
		if k == r.pruneLabel() {
			continue
		}
		if strings.HasPrefix(k, r.cfg.LabelPrefix) {
			if _, keep := want.Labels[k]; !keep {
				delete(updated.Labels, k)
			}
		}
	}
	// Same merge and sweep as the labels above. Only prefixed annotations are
	// swept, so ArgoCD's own and any an operator set by hand survive, which is
	// what the read-modify-write exists for.
	if len(c.annotations) > 0 && updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	maps.Copy(updated.Annotations, c.annotations)
	for k := range updated.Annotations {
		if k == r.pruneLabel() {
			continue
		}
		if strings.HasPrefix(k, r.cfg.LabelPrefix) {
			if _, keep := c.annotations[k]; !keep {
				delete(updated.Annotations, k)
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
// Deleting would throw away what the read-modify-write in apply() protects:
// ArgoCD's own keys and any operator annotations.
//
// ArgoCD discovers clusters purely by the argocd.argoproj.io/secret-type label,
// so parking that one key deregisters immediately while every byte survives. It
// self-heals too: apply() restores secret-type and its prefixed-label sweep
// removes the three labels written here, so a reverted rename needs no special
// case.
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
	// A breadcrumb only. "cluster-" plus a 63-byte cluster name is 71, which the
	// apiserver rejects as a label value, and dropping it beats failing the write:
	// parking the ArgoCD label is the load-bearing half.
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

// changed reports whether the live Secret differs from what we would write,
// comparing stored Data against the StringData we set.
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
	// A label removed upstream must disappear here too. The prune opt-out is
	// exempt for the same reason apply does not sweep it: it is set here, not
	// upstream, so reading it as drift rewrites every pinned registration.
	for k := range existing.Labels {
		if k == PruneLabel(labelPrefix) {
			continue
		}
		if strings.HasPrefix(k, labelPrefix) {
			if _, ok := want.Labels[k]; !ok {
				return true
			}
		}
	}
	// Annotations the same way, both directions. Only prefixed ones are compared:
	// an operator-set or ArgoCD-set annotation is not drift.
	for k, v := range want.Annotations {
		if existing.Annotations[k] != v {
			return true
		}
	}
	for k := range existing.Annotations {
		if k == PruneLabel(labelPrefix) {
			continue
		}
		if strings.HasPrefix(k, labelPrefix) {
			if _, ok := want.Annotations[k]; !ok {
				return true
			}
		}
	}
	return false
}

// collectOne reconciles the registrations recorded against ONE source namespace.
// `exists` is the caller's proof about that namespace, from a single uncached
// read. `applied` is the Secret this namespace wrote this pass, or "".
//
// Deletion requires positive proof: only `exists == false`, set on a definite
// NotFound. Absence from `applied` is never evidence, because a namespace can
// fail to register for reasons that have nothing to do with its cluster being
// gone. The caller must not call this at all for a namespace it could not
// evaluate.
//
// `applied` is at most one Secret, but the set can hold several: a rename leaves
// the old registration behind, demoted, still recorded against this namespace.
//
// The int reports how many owned Secrets still record this namespace. Zero, with
// a namespace provably gone, means there is nothing to come back for.
func (r *Registrar) collectOne(ctx context.Context, nsName string, exists bool, applied string) (int, error) {
	// Ownership and source ALONE: selecting on secret-type would hide demoted
	// registrations, which still need collecting when their namespace goes away.
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

		// Checked before BOTH removal paths: demotion is reversible but still
		// takes a cluster out of ArgoCD's sight, which is what pinning forbids.
		if s.Labels[r.pruneLabel()] == PruneDisabled {
			r.log.Debug("registration is opted out of collection; leaving it",
				slog.String("secret", s.Name), slog.String("namespace", nsName),
				slog.String("label", r.pruneLabel()))
			continue
		}

		if !exists {
			removed, err := r.deleteOrphan(ctx, s, nsName)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			// Only when genuinely gone. A UID conflict leaves the Secret in place,
			// and counting it as removed would retire a key nothing can re-enqueue.
			if removed {
				remaining--
			}
			continue
		}

		// A terminating namespace reaches here with applied == "", skipped by the
		// caller without being marked unevaluable. Demoting on that would hide a
		// live registration the instant its namespace began terminating, and leave
		// it hidden if a finalizer then aborted the deletion.
		if applied == "" {
			r.log.Warn("source namespace still exists but produced no registration; not deleting",
				slog.String("secret", s.Name), slog.String("namespace", nsName))
			continue
		}

		if s.Labels[r.orphanedSecretTypeLabel()] != "" {
			// Demoted on an earlier pass. Kept until it outlives the TTL, if one is
			// set. Below the applied == "" guard on purpose: a terminating namespace
			// can still come back, which a demotion survives and a delete does not.
			// No error return: every uncertain answer is "keep it", so there is
			// nothing a caller could do differently.
			if !r.demotedTTLExpired(s) {
				r.log.Debug("superseded registration is already demoted",
					slog.String("secret", s.Name), slog.String("supersededBy", applied))
				continue
			}
			if err := r.deleteExpired(ctx, s); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			remaining--
			continue
		}

		// The namespace registered under a DIFFERENT name, disclaiming this one.
		// Positive proof of a second kind. Left alone the stale Secret is not
		// inert: never rewritten, it keeps working from a frozen kubeconfig, and
		// ArgoCD picks between two registrations on one server URL arbitrarily.
		if err := r.demote(ctx, s, applied); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return remaining, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return remaining, nil
}

// demotedTTLExpired reports whether a demoted registration has outlived
// Config.DemotedTTL. Every uncertain answer is "no": time.Parse returns the ZERO
// time on failure, so an ignored error would age the fleet by two millennia.
func (r *Registrar) demotedTTLExpired(s *coreV1.Secret) bool {
	if r.cfg.DemotedTTL <= 0 {
		return false
	}
	stamp := s.Labels[r.staleSinceLabel()]
	if stamp == "" {
		// Demoted before the label existed, or by hand.
		r.log.Debug("demoted registration carries no stale-since stamp; it will not expire",
			slog.String("secret", s.Name), slog.String("label", r.staleSinceLabel()))
		return false
	}
	since, err := time.Parse(staleSinceFormat, stamp)
	if err != nil {
		// Warn: keeping it is safe, but indistinguishable from a broken TTL.
		r.log.Warn("demoted registration has an unreadable stale-since stamp; it will not expire",
			slog.String("secret", s.Name), slog.String("staleSince", stamp),
			slog.Any("error", err))
		return false
	}
	// A future stamp gives a negative age, so clock skew needs no special case.
	return time.Since(since) > r.cfg.DemotedTTL
}

// deleteExpired removes a demoted registration that has outlived its TTL.
// Preconditioned on UID *and* resourceVersion, unlike deleteOrphan: the race
// here is a reverted rename, which goes through apply's read-modify-write and so
// preserves the UID. Only the resourceVersion moves.
func (r *Registrar) deleteExpired(ctx context.Context, s *coreV1.Secret) error {
	if r.cfg.DryRun {
		r.log.Info("[dry-run] would delete expired demoted cluster secret",
			slog.String("secret", s.Name), slog.String("staleSince", s.Labels[r.staleSinceLabel()]))
		return nil
	}
	rv := s.ResourceVersion
	err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).Delete(ctx, s.Name, metaV1.DeleteOptions{
		Preconditions: &metaV1.Preconditions{UID: &s.UID, ResourceVersion: &rv},
	})
	switch {
	case err == nil:
		r.log.Info("deleted a demoted registration that outlived its TTL",
			slog.String("secret", s.Name), slog.String("cluster", s.Labels[r.clusterLabel()]),
			slog.String("staleSince", s.Labels[r.staleSinceLabel()]),
			slog.Duration("ttl", r.cfg.DemotedTTL))
		return nil
	case apiErrors.IsNotFound(err):
		return nil
	case apiErrors.IsConflict(err):
		// Written between the read and the delete, most likely a reverted rename
		// restoring it. Leave it; the next pass reads the new state and decides
		// again.
		r.log.Info("demoted registration changed before its TTL delete; leaving it",
			slog.String("secret", s.Name))
		return nil
	default:
		return fmt.Errorf("delete expired %s: %w", s.Name, err)
	}
}

// collectStaleUID removes registrations whose recorded source namespace has been
// replaced by a different object of the same name. Names are reusable, UIDs are
// not, so a recorded UID that no longer matches is positive proof the source is
// gone -- something else holds its name -- which is why this may delete.
//
// It runs BEFORE discovery: delete and recreate a namespace, as a GitOps
// re-apply does, and if the replacement never becomes discoverable every other
// path returns early, leaving the predecessor pointing at a destroyed API server.
//
// Guarded on the label being PRESENT; absence must never read as a mismatch.
func (r *Registrar) collectStaleUID(ctx context.Context, nsName string, uid types.UID) error {
	if uid == "" {
		return nil
	}
	selector := fmt.Sprintf("%s=%s,%s=%s",
		r.managedByLabel(), r.cfg.ManagedByValue, r.sourceNamespaceLabel(), nsName)
	secrets, err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).
		List(ctx, metaV1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list cluster secrets (%s): %w", selector, err)
	}

	var errs []string
	for i := range secrets.Items {
		s := &secrets.Items[i]
		recorded := s.Labels[r.sourceNamespaceUIDLabel()]
		if recorded == "" || recorded == string(uid) {
			continue
		}
		if s.Labels[r.pruneLabel()] == PruneDisabled {
			r.log.Debug("registration is opted out of collection; leaving it",
				slog.String("secret", s.Name), slog.String("namespace", nsName))
			continue
		}
		r.log.Info("source namespace was replaced by a different one of the same name",
			slog.String("secret", s.Name), slog.String("namespace", nsName),
			slog.String("recordedUID", recorded), slog.String("currentUID", string(uid)))
		// The bool is unused: this reports only errors.
		if _, err := r.deleteOrphan(ctx, s, nsName); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// deleteOrphan removes a registration whose source namespace is provably gone.
// The bool reports whether the object is gone, which is not the same as "no error
// occurred": a UID conflict is reported as success because it needs no retry, and
// the caller must not count it as one fewer registration.
func (r *Registrar) deleteOrphan(ctx context.Context, s *coreV1.Secret, nsName string) (bool, error) {
	if r.cfg.DryRun {
		r.log.Info("[dry-run] would delete orphaned cluster secret",
			slog.String("secret", s.Name), slog.String("namespace", nsName))
		return false, nil
	}
	// Preconditioned on the Secret's own UID: under a watch the gap between the
	// existence proof and this call is event latency plus a requeue, so a delete
	// decided in one reconcile could land on a Secret a later one recreated.
	err := r.client.CoreV1().Secrets(r.cfg.TargetNamespace).Delete(ctx, s.Name, metaV1.DeleteOptions{
		Preconditions: &metaV1.Preconditions{UID: &s.UID},
	})
	switch {
	case err == nil:
		r.log.Info("deregistered cluster (source namespace gone)",
			slog.String("secret", s.Name), slog.String("namespace", nsName))
		return true, nil
	case apiErrors.IsNotFound(err):
		// Already gone, by whatever hand.
		return true, nil
	case apiErrors.IsConflict(err):
		// Replaced between the read and the delete, so no longer the object the
		// proof was about. The next reconcile decides again.
		r.log.Info("cluster secret changed identity before deletion; leaving it",
			slog.String("secret", s.Name), slog.String("namespace", nsName))
		return false, nil
	default:
		// One Secret held up by a finalizer must not stall the rest.
		return false, fmt.Errorf("delete %s: %w", s.Name, err)
	}
}
