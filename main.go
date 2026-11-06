// Package main represents the entry point of the application.
package main

import (
	"fmt"
	"os"

	"github.com/pcanilho/argocd-cluster-registrar/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err.Error())
		os.Exit(1)
	}
}
