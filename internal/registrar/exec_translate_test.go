package registrar

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustTranslate(t *testing.T, kc string) argoClusterConfig {
	t.Helper()
	pk, err := parseKubeconfig([]byte(kc), true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got argoClusterConfig
	if err := json.Unmarshal([]byte(pk.config), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return got
}

func execKubeconfigWith(command string, args ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nclusters:\n- name: c\n  cluster:\n" +
		"    server: https://a\n    certificate-authority-data: Y2E=\n" +
		"users:\n- name: u\n  user:\n    exec:\n" +
		"      apiVersion: client.authentication.k8s.io/v1beta1\n" +
		"      command: " + command + "\n")
	if len(args) > 0 {
		b.WriteString("      args:\n")
		for _, a := range args {
			b.WriteString("      - " + a + "\n")
		}
	}
	return b.String()
}

// CAPA's default TokenMethod. awsAuthConfig, not execProviderConfig, because
// ArgoCD hardcodes the command on its own side for that shape.
func TestTranslatesCAPAIamAuthenticator(t *testing.T) {
	got := mustTranslate(t, execKubeconfig)
	if got.AWSAuthConfig == nil {
		t.Fatalf("no awsAuthConfig: %+v", got)
	}
	if got.AWSAuthConfig.ClusterName != "my-eks-cluster" {
		t.Errorf("clusterName = %q", got.AWSAuthConfig.ClusterName)
	}
	if got.ExecProviderConfig != nil {
		t.Error("both credential shapes were emitted; ArgoCD reads only the first")
	}
	if got.BearerToken != "" {
		t.Error("a bearer token was emitted alongside an exec credential")
	}
	if got.TLSClientConfig.CaData != "Y2FkYXRh" {
		t.Errorf("caData was lost: %q", got.TLSClientConfig.CaData)
	}
}

// `aws eks update-kubeconfig` puts --region first, before the subcommand.
// awsAuthConfig has no env field, so a region forces the execProviderConfig
// shape instead.
func TestRegionForcesExecProviderShape(t *testing.T) {
	got := mustTranslate(t, execKubeconfigWith("aws",
		"--region", "eu-west-1", "eks", "get-token", "--cluster-name", "prod"))
	if got.ExecProviderConfig == nil {
		t.Fatalf("no execProviderConfig: %+v", got)
	}
	if got.AWSAuthConfig != nil {
		t.Error("both shapes were emitted")
	}
	if got.ExecProviderConfig.Env["AWS_REGION"] != "eu-west-1" {
		t.Errorf("env = %v", got.ExecProviderConfig.Env)
	}
	if got.ExecProviderConfig.Command != argoAuthCommand {
		t.Errorf("command = %q, want %q", got.ExecProviderConfig.Command, argoAuthCommand)
	}
}

// The command is ours, never the source's. ArgoCD passes execProviderConfig.command
// to os/exec with no allowlist, so echoing one back would be arbitrary code
// execution in the ArgoCD control plane.
func TestEmittedCommandIsNeverTheSourceCommand(t *testing.T) {
	for cmd, args := range map[string][]string{
		"aws-iam-authenticator": {"token", "-i", "c", "--region", "eu-west-1"},
		"kubelogin":             {"get-token", "--server-id", "s"},
		"aws":                   {"--region", "eu-west-1", "eks", "get-token", "--cluster-name", "c"},
	} {
		got := mustTranslate(t, execKubeconfigWith(cmd, args...))
		if got.ExecProviderConfig == nil {
			t.Fatalf("%s: no execProviderConfig", cmd)
		}
		if got.ExecProviderConfig.Command != argoAuthCommand {
			t.Errorf("%s: command = %q, want %q", cmd, got.ExecProviderConfig.Command, argoAuthCommand)
		}
	}
}

// The env map carries the target's identity only. Anything describing the caller
// would let a source namespace choose which identity ArgoCD presents, and
// client-go appends this map AFTER os.Environ so it overrides the pod's own.
func TestKubeloginCarriesTargetIdentityOnly(t *testing.T) {
	got := mustTranslate(t, execKubeconfigWith("kubelogin",
		"get-token", "--login", "devicecode",
		"--server-id", "6dae42f8-4368-4678-94ff-3960e28e3630",
		"--client-id", "SHOULD-NOT-TRAVEL",
		"--tenant-id", "SHOULD-NOT-TRAVEL",
		"--environment", "AzurePublicCloud"))
	if got.ExecProviderConfig == nil {
		t.Fatal("no execProviderConfig")
	}
	if got.ExecProviderConfig.Env["AAD_SERVER_APPLICATION_ID"] == "" {
		t.Error("the AAD server application id was not carried")
	}
	if got.ExecProviderConfig.Env["AAD_ENVIRONMENT_NAME"] != "AzurePublicCloud" {
		t.Error("the cloud environment was not carried")
	}
	for _, forbidden := range []string{
		"AZURE_CLIENT_ID", "AZURE_TENANT_ID", "AAD_LOGIN_METHOD",
		"HTTPS_PROXY", "AZURE_FEDERATED_TOKEN_FILE",
	} {
		if _, ok := got.ExecProviderConfig.Env[forbidden]; ok {
			t.Errorf("%s reached the emitted env", forbidden)
		}
	}
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "SHOULD-NOT-TRAVEL") {
		t.Errorf("caller identity reached the config blob: %s", blob)
	}
}

// gcloud rewrites command to an absolute path when probing the plugin fails.
func TestExecCommandIsMatchedByBaseName(t *testing.T) {
	got := mustTranslate(t, execKubeconfigWith("/usr/local/bin/aws-iam-authenticator",
		"token", "-i", "c"))
	if got.AWSAuthConfig == nil {
		t.Fatal("an absolute command path was not recognised")
	}
}

// heptio-authenticator-aws is the pre-rename spelling and still appears in older
// kubeconfigs. It shares translateIAMAuthenticator, so this pins the alias rather
// than the translation.
func TestHeptioSpellingTranslatesLikeIamAuthenticator(t *testing.T) {
	got := mustTranslate(t, execKubeconfigWith("heptio-authenticator-aws",
		"token", "-i", "my-eks-cluster"))
	if got.AWSAuthConfig == nil {
		t.Fatalf("the legacy spelling was not translated: %+v", got)
	}
	if got.AWSAuthConfig.ClusterName != "my-eks-cluster" {
		t.Errorf("clusterName = %q", got.AWSAuthConfig.ClusterName)
	}
	if got.ExecProviderConfig != nil {
		t.Error("both credential shapes were emitted")
	}
}

func TestRefusesUntranslatableExec(t *testing.T) {
	for name, kc := range map[string]string{
		// argocd-k8s-auth gcp returns an OAuth token scoped cloud-platform for
		// ArgoCD's own service account, bound to no cluster and no audience.
		"gke plugin is not shipped": execKubeconfigWith("gke-gcloud-auth-plugin"),
		"unknown command":           execKubeconfigWith("oidc-login", "get-token"),
		"aws without eks get-token": execKubeconfigWith("aws", "sts", "get-caller-identity"),
		"no cluster name":           execKubeconfigWith("aws-iam-authenticator", "token"),
		// Dropping these silently would change which identity the token is minted
		// for, or turn a working AssumeRole into AccessDenied with no clue why.
		"external id":  execKubeconfigWith("aws-iam-authenticator", "token", "-i", "c", "-e", "x"),
		"session name": execKubeconfigWith("aws-iam-authenticator", "token", "-i", "c", "--session-name", "x"),
		"role arn":     execKubeconfigWith("aws-iam-authenticator", "token", "-i", "c", "-r", "arn:aws:iam::1:role/x"),
		"aws profile":  execKubeconfigWith("aws", "eks", "get-token", "--cluster-name", "c", "--profile", "dev"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseKubeconfig([]byte(kc), true); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// v1alpha1 was removed from client-go, and ArgoCD copies apiVersion verbatim, so
// this would fail at connect time with "exec plugin: invalid apiVersion".
func TestRefusesUnsupportedExecAPIVersion(t *testing.T) {
	kc := strings.Replace(execKubeconfig,
		"client.authentication.k8s.io/v1beta1", "client.authentication.k8s.io/v1alpha1", 1)
	if _, err := parseKubeconfig([]byte(kc), true); err == nil {
		t.Error("accepted a removed apiVersion")
	}
}

// The gate is what makes the refusal a decision rather than an accident.
func TestExecIsRefusedWhenTranslationIsOff(t *testing.T) {
	if _, err := parseKubeconfig([]byte(execKubeconfig), false); err == nil {
		t.Fatal("an exec credential was translated with the gate shut")
	}
}

// A static credential needs no ambient identity, so it wins where a kubeconfig
// carries both. ArgoCD ignores bearerToken when either exec field is set, so
// emitting both would put a credential in the Secret that is never used.
func TestStaticCredentialWinsOverExec(t *testing.T) {
	kc := "apiVersion: v1\nclusters:\n- name: c\n  cluster:\n    server: https://a\n" +
		"    certificate-authority-data: Y2E=\nusers:\n- name: u\n  user:\n" +
		"    token: statictoken\n    exec:\n" +
		"      apiVersion: client.authentication.k8s.io/v1beta1\n" +
		"      command: aws-iam-authenticator\n      args: [token, -i, c]\n"
	got := mustTranslate(t, kc)
	if got.BearerToken != "statictoken" {
		t.Errorf("bearerToken = %q", got.BearerToken)
	}
	if got.AWSAuthConfig != nil || got.ExecProviderConfig != nil {
		t.Error("an exec credential was emitted alongside a static one")
	}
}
