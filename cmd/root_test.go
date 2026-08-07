package cmd

import (
	"strings"
	"testing"

	"github.com/pcanilho/argocd-cluster-registrar/internal/registrar"
	"github.com/spf13/cobra"
)

// parseFlags builds a command carrying the REAL flag definitions and parses args
// through it, so these tests exercise registerFlags rather than a copy of it.
//
// The flags bind to package-level variables, so each call must start from a clean
// set or one case leaks into the next. Re-registering onto a fresh FlagSet resets
// every binding to its declared default, which is exactly what the `Changed` gate
// depends on.
func parseFlags(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test", RunE: func(*cobra.Command, []string) error { return nil }}
	registerFlags(cmd.PersistentFlags())
	// ParseFlags, not PersistentFlags().Parse: only the former merges persistent
	// flags into cmd.Flags(), which is the set resolveProviders asks about. Parsing
	// the other way leaves Changed() false there and every legacy case silently
	// falls through to the default -- which is the bug this file exists to catch.
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return cmd
}

func names(providers []registrar.Provider) []string {
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		out = append(out, p.Name)
	}
	return out
}

// Nothing set: the shipped default, unchanged since 0.2.x.
func TestResolveProvidersDefaultsToK3k(t *testing.T) {
	got, err := resolveProviders(parseFlags(t))
	if err != nil {
		t.Fatalf("resolveProviders: %v", err)
	}
	if len(got) != 1 || got[0].Name != "k3k" {
		t.Errorf("default = %v, want [k3k]", names(got))
	}
}

// The distinction the whole compatibility path rests on. --secret-name-pattern and
// --secret-key carry NON-EMPTY defaults, so a gate testing them for "" would take
// the legacy branch every single time and silently ignore --provider. This asserts
// the gate is `Changed`, not emptiness: the deprecated flags are at their defaults
// here and must not win.
func TestResolveProvidersIgnoresUnsetDeprecatedDefaults(t *testing.T) {
	cmd := parseFlags(t, "--provider=capi")

	if pattern := cmd.Flags().Lookup("secret-name-pattern").Value.String(); pattern == "" {
		t.Fatal("precondition: secret-name-pattern is expected to have a non-empty default")
	}

	got, err := resolveProviders(cmd)
	if err != nil {
		t.Fatalf("resolveProviders: %v", err)
	}
	if len(got) != 1 || got[0].Name != "capi" {
		t.Fatalf("got %v, want [capi]; the unset deprecated defaults took over", names(got))
	}
	if got[0].SecretNamePattern != "*-kubeconfig" {
		t.Errorf("pattern = %q, want the capi preset's", got[0].SecretNamePattern)
	}
}

// A 0.2.x invocation keeps working.
func TestResolveProvidersHonoursDeprecatedFlags(t *testing.T) {
	got, err := resolveProviders(parseFlags(t,
		"--secret-name-pattern=vc-*", "--secret-key=config"))
	if err != nil {
		t.Fatalf("resolveProviders: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d providers, want 1", len(got))
	}
	if got[0].SecretNamePattern != "vc-*" || len(got[0].SecretKeys) != 1 || got[0].SecretKeys[0] != "config" {
		t.Errorf("legacy flags not honoured: %+v", got[0])
	}
}

// Setting only one of the pair still takes the legacy path, because the gate is
// per-flag. The other keeps its default, which is the documented "set BOTH or
// neither" contract failing loudly rather than silently.
func TestResolveProvidersLegacyGateIsPerFlag(t *testing.T) {
	for _, arg := range []string{"--secret-name-pattern=vc-*", "--secret-key=config"} {
		t.Run(arg, func(t *testing.T) {
			got, err := resolveProviders(parseFlags(t, arg))
			if err != nil {
				t.Fatalf("resolveProviders: %v", err)
			}
			if got[0].Name != "custom" {
				t.Errorf("got %v, want the legacy custom provider", names(got))
			}
		})
	}
}

// Mixing the two forms is refused rather than silently preferring one, which is
// how an upgrade ends up looking healthy while registering nothing.
func TestResolveProvidersRejectsMixedForms(t *testing.T) {
	_, err := resolveProviders(parseFlags(t, "--provider=k3k", "--secret-name-pattern=vc-*"))
	if err == nil {
		t.Fatal("expected an error when both forms are set")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("error does not explain the conflict: %v", err)
	}
}

// --provider is repeatable, and order is preserved because it decides precedence
// where two providers could claim the same Secret.
func TestResolveProvidersPreservesOrder(t *testing.T) {
	got, err := resolveProviders(parseFlags(t, "--provider=capi", "--provider=k3k"))
	if err != nil {
		t.Fatalf("resolveProviders: %v", err)
	}
	if want := []string{"capi", "k3k"}; strings.Join(names(got), ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", names(got), want)
	}
}

// StringArray, not StringSlice: a custom spec's key list is comma-separated, and
// StringSlice would split "a,b" into two providers.
func TestResolveProvidersDoesNotSplitCustomKeyLists(t *testing.T) {
	got, err := resolveProviders(parseFlags(t, "--provider=mytool=my-*-kubeconfig=admin.conf,admin.svc"))
	if err != nil {
		t.Fatalf("resolveProviders: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d providers, want 1; the key list was split", len(got))
	}
	if len(got[0].SecretKeys) != 2 {
		t.Errorf("keys = %v, want both", got[0].SecretKeys)
	}
}

func TestResolveProvidersRejectsBadSpecs(t *testing.T) {
	for name, spec := range map[string]string{
		"unknown preset": "nosuchprovider",
		"too few fields": "mytool=my-*",
		"too many":       "a=b=c=d",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveProviders(parseFlags(t, "--provider="+spec)); err == nil {
				t.Errorf("accepted %q", spec)
			}
		})
	}
}

// The resolved providers must survive Validate, or a bad flag is accepted at
// startup and then rejected by the apiserver on every apply, forever.
func TestResolvedProvidersPassValidation(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--provider=k3k", "--provider=capi"},
		{"--secret-name-pattern=vc-*", "--secret-key=config"},
	} {
		providers, err := resolveProviders(parseFlags(t, args...))
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		cfg := registrar.Config{
			TargetNamespace: "argocd",
			ManagedByValue:  "cluster-registrar",
			Providers:       providers,
			LabelPrefix:     registrar.DefaultLabelPrefix,
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%v produced a config that fails validation: %v", args, err)
		}
	}
}
