package registrar

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
)

// kubeconfigExec is a credential plugin block. Typed now rather than `any`,
// because translating one means reading it.
type kubeconfigExec struct {
	APIVersion string   `yaml:"apiVersion"`
	Command    string   `yaml:"command"`
	Args       []string `yaml:"args"`
	Env        []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
}

// argoAWSAuthConfig mirrors ArgoCD's `config.awsAuthConfig`. ArgoCD turns it into
// `argocd-k8s-auth aws --cluster-name X`, with the command hardcoded on its side.
//
// RoleARN and Profile are absent on purpose. ArgoCD assumes a role with its own
// identity, so a role named by the source kubeconfig would let a namespace pick
// which role ArgoCD assumes. Profile resolves against ArgoCD's filesystem, not
// the caller's, so carrying it authenticates as a different principal or none.
type argoAWSAuthConfig struct {
	ClusterName string `json:"clusterName"`
}

// argoExecProviderConfig mirrors ArgoCD's `config.execProviderConfig`.
//
// Command is written by us and never read from the source. ArgoCD passes it
// straight to os/exec with no allowlist, so a kubeconfig-supplied command would
// be arbitrary code execution in the ArgoCD control plane.
type argoExecProviderConfig struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	APIVersion string            `json:"apiVersion"`
}

const (
	// argoAuthCommand is the only credential plugin in the ArgoCD image, which
	// symlinks it to the argocd binary. No cloud CLI is present, so copying a
	// source command through would produce a registration ArgoCD cannot run.
	argoAuthCommand = "argocd-k8s-auth"
	execAPIVersion  = "client.authentication.k8s.io/v1beta1"
)

// execCredential is one translation result: exactly one shape, never both.
// ArgoCD reads awsAuthConfig first and ignores execProviderConfig when it is
// set, so a Secret carrying both would document one thing and do another.
type execCredential struct {
	aws  *argoAWSAuthConfig
	exec *argoExecProviderConfig
}

// execTranslators maps a source credential plugin onto ArgoCD's equivalent.
//
// Keyed on the command's base name, because gcloud rewrites `command` to an
// absolute path whenever probing the plugin fails.
//
// Absent from this table means refused: only shapes whose meaning is known get a
// translation, and everything else keeps the pre-existing refusal.
var execTranslators = map[string]func(*kubeconfigExec) (execCredential, error){
	"aws-iam-authenticator":    translateIAMAuthenticator,
	"heptio-authenticator-aws": translateIAMAuthenticator,
	"aws":                      translateAWSCLI,
	"kubelogin":                translateKubelogin,
	// gke-gcloud-auth-plugin is deliberately absent. `argocd-k8s-auth gcp`
	// returns an OAuth token scoped cloud-platform for ArgoCD's own service
	// account, bound to no cluster and no audience, sent to whatever `server`
	// says. AWS and Azure at least mint cluster-bound credentials.
}

func translatableCommands() []string {
	out := make([]string, 0, len(execTranslators))
	for k := range execTranslators {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// translateExec turns a source exec block into ArgoCD's equivalent.
func translateExec(e *kubeconfigExec) (execCredential, error) {
	if e.APIVersion != "" &&
		e.APIVersion != execAPIVersion &&
		e.APIVersion != "client.authentication.k8s.io/v1" {
		return execCredential{}, fmt.Errorf(
			"kubeconfig exec declares apiVersion %q; client-go accepts only "+
				"client.authentication.k8s.io/v1beta1 and .../v1", e.APIVersion)
	}
	cmd := path.Base(strings.TrimSpace(e.Command))
	fn, ok := execTranslators[cmd]
	if !ok {
		return execCredential{}, fmt.Errorf(
			"kubeconfig exec command %q has no ArgoCD equivalent; only %s can be translated, "+
				"and copying the command through would produce a registration ArgoCD cannot "+
				"run, because its image ships %s and no cloud CLI",
			e.Command, strings.Join(translatableCommands(), ", "), argoAuthCommand)
	}
	return fn(e)
}

// execFlag returns the value following the first of names present in args.
func execFlag(args []string, names ...string) (string, bool) {
	for i, a := range args {
		for _, n := range names {
			if a == n && i+1 < len(args) {
				return args[i+1], true
			}
			if v, found := strings.CutPrefix(a, n+"="); found {
				return v, true
			}
		}
	}
	return "", false
}

// refuseUnsupportedArgs rejects anything outside the allowlist rather than
// dropping it. Silently dropping an --external-id turns a working AssumeRole
// into AccessDenied with no clue why.
func refuseUnsupportedArgs(args, allowed []string, cmd string) error {
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[a] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, _ := strings.Cut(a, "=")
		if ok[name] {
			// Skip its value when it was given as a separate argument.
			if !strings.Contains(a, "=") && i+1 < len(args) {
				i++
			}
			continue
		}
		return fmt.Errorf(
			"kubeconfig exec passes %q to %s, which %s does not accept; dropping it "+
				"would change which identity the token is minted for, so this refuses instead",
			name, cmd, argoAuthCommand)
	}
	return nil
}

// translateIAMAuthenticator handles `aws-iam-authenticator token -i <cluster>`,
// which is CAPA's default for a managed EKS control plane.
func translateIAMAuthenticator(e *kubeconfigExec) (execCredential, error) {
	if err := refuseUnsupportedArgs(e.Args,
		[]string{"-i", "--cluster-id", "--region"}, "aws-iam-authenticator"); err != nil {
		return execCredential{}, err
	}
	name, ok := execFlag(e.Args, "-i", "--cluster-id")
	if !ok {
		return execCredential{}, fmt.Errorf(
			"kubeconfig exec runs aws-iam-authenticator with no -i/--cluster-id; ArgoCD needs " +
				"the EKS cluster name and it cannot be inferred from the Secret, whose name is " +
				"the Cluster API cluster name and may differ")
	}
	region, _ := execFlag(e.Args, "--region")
	return awsCredential(name, region), nil
}

// translateAWSCLI handles `aws [--region R] eks get-token --cluster-name N`,
// which is what `aws eks update-kubeconfig` writes.
func translateAWSCLI(e *kubeconfigExec) (execCredential, error) {
	if !hasSubcommand(e.Args, "eks", "get-token") {
		return execCredential{}, fmt.Errorf(
			"kubeconfig exec runs `aws` but not `eks get-token` (args: %s); nothing else the "+
				"AWS CLI prints is an ExecCredential ArgoCD can use", strings.Join(e.Args, " "))
	}
	if err := refuseUnsupportedArgs(e.Args,
		[]string{"--region", "--cluster-name", "--cluster-id", "--output"}, "aws"); err != nil {
		return execCredential{}, err
	}
	name, ok := execFlag(e.Args, "--cluster-name", "--cluster-id")
	if !ok {
		return execCredential{}, fmt.Errorf(
			"kubeconfig exec runs `aws eks get-token` with no --cluster-name; ArgoCD needs the " +
				"EKS cluster name and it cannot be inferred from the Secret")
	}
	region, _ := execFlag(e.Args, "--region")
	return awsCredential(name, region), nil
}

// awsCredential picks which of ArgoCD's two shapes to emit.
//
// awsAuthConfig has no env field and argocd-k8s-auth resolves the region only
// from its own process environment, so a fleet spanning regions cannot be
// expressed any other way. The region is what decides the shape.
func awsCredential(clusterName, region string) execCredential {
	if region == "" {
		return execCredential{aws: &argoAWSAuthConfig{ClusterName: clusterName}}
	}
	return execCredential{exec: &argoExecProviderConfig{
		Command:    argoAuthCommand,
		Args:       []string{"aws", "--cluster-name", clusterName},
		Env:        map[string]string{"AWS_REGION": region},
		APIVersion: execAPIVersion,
	}}
}

// translateKubelogin handles AKS.
//
// The env map carries the target's identity only: which AAD server application,
// which cloud. --login, --client-id, --tenant-id and the credential flags
// describe the caller, and how ArgoCD authenticates is a property of the ArgoCD
// deployment, set in its pod spec where it applies uniformly. Carrying them from
// a source kubeconfig would let a namespace choose which identity ArgoCD
// presents, and `--login devicecode` or `azurecli` cannot work in a pod anyway.
//
//nolint:unparam // the error is required by execTranslators; kubelogin cannot fail
func translateKubelogin(e *kubeconfigExec) (execCredential, error) {
	env := map[string]string{}
	if v, ok := execFlag(e.Args, "--server-id"); ok {
		env["AAD_SERVER_APPLICATION_ID"] = v
	}
	if v, ok := execFlag(e.Args, "--environment"); ok {
		env["AAD_ENVIRONMENT_NAME"] = v
	}
	if len(env) == 0 {
		env = nil
	}
	return execCredential{exec: &argoExecProviderConfig{
		Command:    argoAuthCommand,
		Args:       []string{"azure"},
		Env:        env,
		APIVersion: execAPIVersion,
	}}, nil
}

// hasSubcommand reports whether the positional arguments contain the given
// sequence in order, which is how `eks get-token` is recognised past --region.
func hasSubcommand(args []string, want ...string) bool {
	positional := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			if !strings.Contains(args[i], "=") {
				i++ // its value
			}
			continue
		}
		positional = append(positional, args[i])
	}
	for i := 0; i+len(want) <= len(positional); i++ {
		if slices.Equal(positional[i:i+len(want)], want) {
			return true
		}
	}
	return false
}
