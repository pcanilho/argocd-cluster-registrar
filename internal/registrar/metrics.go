package registrar

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics for the outcomes a log line alone cannot be alerted on.
//
// WHY THESE ARE PACKAGE-LEVEL rather than a field on Registrar, which is how the
// logger is threaded: controller-runtime's Registry is a process global and
// MustRegister PANICS on a duplicate. One Registrar per process makes an instance
// field buy nothing, and the test binary builds dozens of them by struct literal,
// so registering in a constructor would panic on the second one and a field left
// nil would panic in production on whichever path forgot to guard it.
//
// WHY THE LABELS ARE ALL CONSTANTS. Every label value below comes from a closed
// set fixed at compile time. Nothing here is a cluster name, a namespace or
// anything else read off an object a tenant can create. That is deliberate on the
// conflict path especially, which is the one path that exists because somebody
// may be acting in bad faith: labelling by cluster name would let anyone able to
// label a namespace mint unbounded series in the monitoring system.
//
// WHY THERE IS NO PER-CONFLICT GAUGE. A gauge keyed on the contested name and the
// two namespaces would answer "which cluster is contested right now", and it is
// tempting. It cannot be cleared. When the losing claimant's namespace is
// deleted, ReconcileOne takes the NotFound branch and never reaches apply, so
// nothing is left to zero the series and it alerts forever on a conflict that no
// longer exists. The identity lives in the log line, which carries the cluster,
// both namespaces and the reason. The metric's job is to make that line worth
// looking for.
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
