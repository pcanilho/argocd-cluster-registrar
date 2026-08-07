// Package main represents the entry point of the application.
package main

import (
	"os"

	"github.com/pcanilho/argocd-cluster-registrar/cmd"
)

func main() {
	// No message and no logger. cobra prints the error itself (SilenceErrors is
	// false, deliberately), so wrapping it here printed every failure twice: once
	// as logfmt on stdout like the rest of the process, once with the standard
	// library's date prefix on stderr. The wording was inherited from
	// vcluster-argocd-exporter too, so a flag validation error caught before
	// anything touched a cluster was reported as a failure to export clusters.
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
