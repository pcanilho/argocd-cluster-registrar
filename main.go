// Package main represents the entry point of the application.
package main

import (
	"os"

	"github.com/pcanilho/argocd-cluster-registrar/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
