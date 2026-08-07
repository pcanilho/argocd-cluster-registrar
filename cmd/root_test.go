package cmd

import (
	"fmt"
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

// Every flag must reach the option it configures.
//
// The two struct literals in buildOptions are hand-written, so a field left off
// leaves the option at its zero value and nothing complains. That is not a
// theoretical worry: leaving LeaderElection unset merely disables a feature,
// while leaving a bind address unset hands controller-runtime an empty string,
// which it reads as "serve on the default port" rather than as "off".
//
// Each flag is parsed ALONE. Setting several at once would hide the one
// interaction that exists (--dry-run de-escalating --leader-elect), and a case
// asserting `LeaderElection == true` would then fail for a reason that has
// nothing to do with wiring.
//
// The `want != base` assertion is what stops this passing vacuously: with
// --target-namespace=argocd, a field that is never assigned still reads "argocd"
// and the test would prove nothing.
func TestEveryFlagReachesTheOptionItConfigures(t *testing.T) {
	base, err := buildOptions(parseFlags(t))
	if err != nil {
		t.Fatalf("defaults do not build: %v", err)
	}

	for _, tc := range []struct {
		arg  string
		want string
		got  func(options) string
	}{
		{"--interval=90s", "1m30s",
			func(o options) string { return o.ctrl.Interval.String() }},
		{"--health-probe-bind-address=:9", ":9",
			func(o options) string { return o.ctrl.HealthProbeBindAddress }},
		{"--metrics-bind-address=:9090", ":9090",
			func(o options) string { return o.ctrl.MetricsBindAddress }},
		{"--leader-election-id=some-other-lease", "some-other-lease",
			func(o options) string { return o.ctrl.LeaderElectionID }},
		{"--leader-elect", "true",
			func(o options) string { return fmt.Sprint(o.ctrl.LeaderElection) }},
		{"--target-namespace=somewhere-else", "somewhere-else",
			func(o options) string { return o.cfg.TargetNamespace }},
		{"--managed-by=some-other-value", "some-other-value",
			func(o options) string { return o.cfg.ManagedByValue }},
		{"--label-prefix=example.com/", "example.com/",
			func(o options) string { return o.cfg.LabelPrefix }},
		{"--dry-run", "true",
			func(o options) string { return fmt.Sprint(o.cfg.DryRun) }},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			if d := tc.got(base); d == tc.want {
				t.Fatalf("the test value %q equals the flag's default, so this case "+
					"would pass even if the field were never assigned", tc.want)
			}
			opts, err := buildOptions(parseFlags(t, tc.arg))
			if err != nil {
				t.Fatalf("buildOptions: %v", err)
			}
			if got := tc.got(opts); got != tc.want {
				t.Errorf("%s produced %q, want %q; the flag does not reach its option",
					tc.arg, got, tc.want)
			}
		})
	}
}

// --dry-run must stand a --leader-elect down, and must not write that decision
// back into the flag variable.
//
// The flags bind to package-level variables. Assigning to one from inside the
// run path used to leave it mutated for the rest of the process, which in a test
// binary means every later case inherits it.
func TestDryRunDeEscalatesLeaderElectionWithoutMutatingTheFlag(t *testing.T) {
	opts, err := buildOptions(parseFlags(t, "--dry-run", "--leader-elect"))
	if err != nil {
		t.Fatalf("buildOptions: %v", err)
	}
	if opts.ctrl.LeaderElection {
		t.Error("a --dry-run process would hold the lease while the real registrar waited")
	}
	if !leaderElect {
		t.Error("the --leader-elect flag variable was mutated; that state leaks into " +
			"every later caller in the process")
	}
	if len(opts.warnings) != 1 || !strings.Contains(opts.warnings[0], "leader election") {
		t.Errorf("warnings = %q, want one mentioning leader election", opts.warnings)
	}
}

// A zero or negative interval means RequeueAfter: 0, which controller-runtime
// reads as "never come back". Rejecting it is the difference between a loud
// startup failure and registrations that silently stop refreshing.
func TestANonPositiveIntervalIsRejected(t *testing.T) {
	for _, arg := range []string{"--interval=0s", "--interval=-1s"} {
		if _, err := buildOptions(parseFlags(t, arg)); err == nil {
			t.Errorf("%s was accepted; every registration would be reconciled once "+
				"and then never refreshed", arg)
		}
	}
}

// Metrics stay off unless asked for, and "off" is the string "0".
//
// Not the empty string: controller-runtime reads an empty bind address as
// ":8080" rather than as "off", so defaulting to empty here would open an
// unauthenticated port on every install that never mentioned metrics. The
// manager normalises empty to "0" as well; both are wanted, because either
// alone can be undone by a plausible edit to the other.
func TestMetricsAreOffUnlessAskedFor(t *testing.T) {
	opts, err := buildOptions(parseFlags(t))
	if err != nil {
		t.Fatalf("buildOptions: %v", err)
	}
	if opts.ctrl.MetricsBindAddress != "0" {
		t.Errorf("default metrics bind address = %q, want \"0\"; an empty value "+
			"serves :8080 unauthenticated", opts.ctrl.MetricsBindAddress)
	}
}
