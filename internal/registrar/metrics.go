package registrar

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics for the outcomes a log line alone cannot be alerted on.
//
// Package-level because the Registry is a process global and MustRegister panics
// on a duplicate. Every label value is a compile-time constant: a cluster or
// namespace name would be attacker-chosen cardinality on the conflict path, so
// which cluster is contested stays in the log line.
var (
	conflictsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "argocd_cluster_registrar_conflicts_total",
		Help: "Registrations refused because the cluster name was already spoken for, by reason.",
	}, []string{"reason"})

	adoptionsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "argocd_cluster_registrar_adoptions_total",
		Help: "Owned cluster Secrets recording no source namespace that were adopted by a matching namespace.",
	})

	// A gauge, not a counter: "are demoted registrations piling up" is a level.
	// Set from the audit pass, which already lists every owned Secret.
	registrations = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "argocd_cluster_registrar_registrations",
		Help: "Cluster registrations this instance owns, by state and remaining credential lifetime.",
	}, []string{"state", "credential_expiry"})

	unroutedSecrets = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "argocd_cluster_registrar_unrouted_secrets",
		Help: "Owned cluster Secrets recording no source namespace, which no reconcile key can ever collect.",
	})
)

// The two states a registration can be in. Demoted means hidden from ArgoCD but
// kept, which is what a rename leaves behind.
const (
	stateActive  = "active"
	stateDemoted = "demoted"
)

// The expiry buckets. These are not cumulative: each registration lands in
// exactly one, so `expired|lt_24h` is a sum and an alert can say how many.
const (
	expiryExpired = "expired"
	expiryLt24h   = "lt_24h"
	expiryLt7d    = "lt_7d"
	expiryLt30d   = "lt_30d"
	expiryOK      = "ok"
	// expiryToken is a bearer token: no certificate to read, so unmeasured rather
	// than healthy.
	expiryToken = "token"
	// expiryExec is a translated exec credential, minted per connection, so there
	// is nothing to expire. Also the only signal that translation ran.
	expiryExec = "exec"
	// expiryUnreadable is a certificate or config blob that will not decode.
	expiryUnreadable = "unreadable"
	// expiryAbsent is a registration with no config at all. Nothing is broken.
	expiryAbsent = "absent"
)

var expiryBuckets = []string{
	expiryExpired, expiryLt24h, expiryLt7d, expiryLt30d,
	expiryOK, expiryToken, expiryExec, expiryUnreadable, expiryAbsent,
}

// conflictReasons is every value the reason label can take. One list, so that
// publishing them at zero and asserting on them cannot drift apart.
var conflictReasons = []string{
	conflictNotManaged,
	conflictOrphanClusterMismatch,
	conflictIncumbent,
	conflictContestedName,
	conflictCreateRace,
	conflictServerCollision,
}

func init() {
	ctrlmetrics.Registry.MustRegister(
		conflictsTotal, adoptionsTotal, registrations, unroutedSecrets)

	// A counter never incremented is absent from /metrics entirely, which makes
	// `increase(...[15m]) > 0` unevaluable rather than false, so an alert written
	// against it never fires and never says why.
	for _, reason := range conflictReasons {
		conflictsTotal.WithLabelValues(reason)
	}
	for _, state := range []string{stateActive, stateDemoted} {
		for _, bucket := range expiryBuckets {
			registrations.WithLabelValues(state, bucket)
		}
	}
}
