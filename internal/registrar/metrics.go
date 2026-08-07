package registrar

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics for the outcomes a log line alone cannot be alerted on.
//
// Package-level, not a Registrar field: the Registry is a process global and
// MustRegister panics on a duplicate, so registering per instance would panic on
// the second Registrar the test binary builds.
//
// Every label value below is a compile-time constant. Nothing here is a cluster
// name or a namespace, which on the conflict path would be attacker-chosen
// cardinality. Which cluster is contested stays in the log line, and so does the
// reason a per-conflict gauge is absent: it could never be cleared, since a
// deleted claimant namespace never reaches the code that would zero it.
var (
	conflictsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "argocd_cluster_registrar_conflicts_total",
		Help: "Registrations refused because the cluster name was already spoken for, by reason.",
	}, []string{"reason"})

	adoptionsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "argocd_cluster_registrar_adoptions_total",
		Help: "Owned cluster Secrets recording no source namespace that were adopted by a matching namespace.",
	})

	// registrations is a gauge rather than a counter because the question is "are
	// demoted registrations piling up", which is a level and not a rate. It is set
	// from the audit pass, which already lists every owned Secret, so it costs no
	// extra API call.
	registrations = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "argocd_cluster_registrar_registrations",
		Help: "Cluster registrations this instance owns, by state.",
	}, []string{"state"})

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

// conflictReasons is every value the reason label can take. One list, so that
// publishing them at zero and asserting on them cannot drift apart.
var conflictReasons = []string{
	conflictNotManaged,
	conflictOrphanClusterMismatch,
	conflictIncumbent,
	conflictContestedName,
	conflictCreateRace,
}

func init() {
	ctrlmetrics.Registry.MustRegister(
		conflictsTotal, adoptionsTotal, registrations, unroutedSecrets)

	// Published at zero from the start. A counter that has never been incremented
	// is absent from /metrics entirely, and an absent series makes
	// `increase(...[15m]) > 0` silently unevaluable rather than false, so an alert
	// written against it never fires and never says why.
	for _, reason := range conflictReasons {
		conflictsTotal.WithLabelValues(reason)
	}
	registrations.WithLabelValues(stateActive)
	registrations.WithLabelValues(stateDemoted)
}
