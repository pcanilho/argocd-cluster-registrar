// Package main represents the entry point of the application.
package main

import (
	"log"

	"github.com/pcanilho/argocd-cluster-registrar/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		log.Fatalf("failed to export cluster(s). error: %v", err)
	}
}
