package registrar

import (
	"encoding/json"
	"strings"
	"testing"
)

// Shape taken verbatim from a live k3k v1.2.0-rc3 `k3k-<name>-kubeconfig`
// Secret: client certificates, and a server URL that is the LoadBalancer address
// on port 443 (NOT the in-cluster Service DNS name on 6443).
const k3kKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: sandbox
  cluster:
    server: https://192.168.1.192
    certificate-authority-data: Y2FkYXRh
users:
- name: sandbox
  user:
    client-certificate-data: Y2VydGRhdGE=
    client-key-data: a2V5ZGF0YQ==
contexts:
- name: sandbox
  context:
    cluster: sandbox
    user: sandbox
current-context: sandbox
`

// vcluster issued a bearer token instead. Still supported so the registrar stays
// runtime-agnostic.
const tokenKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: legacy
  cluster:
    server: https://10.0.0.1:6443
    certificate-authority-data: Y2FkYXRh
users:
- name: legacy
  user:
    token: sometoken
`

// Shape taken from a live vcluster 0.36.1 `vc-<name>` Secret, key `config`,
// with exportKubeConfig.server set. Without that setting the server reads
// https://localhost:8443 and the registration is useless to ArgoCD.
const vclusterKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: Y2FkYXRh
    server: https://192.168.1.194
  name: kubernetes
users:
- name: kubernetes-super-admin
  user:
    client-certificate-data: Y2VydGRhdGE=
    client-key-data: a2V5ZGF0YQ==
`

func TestParseKubeconfigClientCerts(t *testing.T) {
	server, config, err := parseKubeconfig([]byte(k3kKubeconfig))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server != "https://192.168.1.192" {
		t.Errorf("server = %q, want https://192.168.1.192", server)
	}

	var got argoClusterConfig
	if err := json.Unmarshal([]byte(config), &got); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	if got.TLSClientConfig.CertData != "Y2VydGRhdGE=" {
		t.Errorf("certData = %q", got.TLSClientConfig.CertData)
	}
	if got.TLSClientConfig.KeyData != "a2V5ZGF0YQ==" {
		t.Errorf("keyData = %q", got.TLSClientConfig.KeyData)
	}
	if got.TLSClientConfig.CaData != "Y2FkYXRh" {
		t.Errorf("caData = %q", got.TLSClientConfig.CaData)
	}
	if got.TLSClientConfig.Insecure {
		t.Error("insecure should be false")
	}
	// A bearerToken alongside client certs would be ambiguous for ArgoCD.
	if got.BearerToken != "" {
		t.Errorf("bearerToken should be empty, got %q", got.BearerToken)
	}
	// omitempty must actually drop it from the JSON, not emit a null.
	if strings.Contains(config, "bearerToken") {
		t.Errorf("bearerToken key should be omitted entirely: %s", config)
	}
}

func TestParseKubeconfigBearerToken(t *testing.T) {
	server, config, err := parseKubeconfig([]byte(tokenKubeconfig))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server != "https://10.0.0.1:6443" {
		t.Errorf("server = %q", server)
	}
	var got argoClusterConfig
	if err := json.Unmarshal([]byte(config), &got); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	if got.BearerToken != "sometoken" {
		t.Errorf("bearerToken = %q", got.BearerToken)
	}
	if got.TLSClientConfig.CertData != "" {
		t.Errorf("certData should be empty, got %q", got.TLSClientConfig.CertData)
	}
}

func TestParseKubeconfigErrors(t *testing.T) {
	for name, in := range map[string]string{
		"not yaml":       "\t\x00not: [yaml",
		"no clusters":    "apiVersion: v1\nusers:\n- name: a\n  user:\n    token: t\n",
		"no users":       "apiVersion: v1\nclusters:\n- name: a\n  cluster:\n    server: https://x\n",
		"no credentials": "apiVersion: v1\nclusters:\n- name: a\n  cluster:\n    server: https://x\nusers:\n- name: a\n  user: {}\n",
		// A cert without its key is unusable and must not silently produce a
		// credential-less cluster Secret that ArgoCD would fail on later.
		"cert without key": "apiVersion: v1\nclusters:\n- name: a\n  cluster:\n    server: https://x\nusers:\n- name: a\n  user:\n    client-certificate-data: Y2VydA==\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseKubeconfig([]byte(in)); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestPropagatedLabels(t *testing.T) {
	const p = "nas.canilho.net/"
	got := propagatedLabels(map[string]string{
		ManagedByLabel(p):             "cluster-registrar", // describes the source
		ClusterLabel(p):               "sandbox",           // set explicitly by apply()
		"nas.canilho.net/flux":        "true",              // must reach the cluster Secret
		"nas.canilho.net/env":         "lab",
		"kubernetes.io/metadata.name": "k3k-sandbox", // unrelated, must not leak
	}, p)
	if len(got) != 2 {
		t.Fatalf("expected 2 propagated labels, got %d: %v", len(got), got)
	}
	if got["nas.canilho.net/flux"] != "true" {
		t.Errorf("flux label not propagated: %v", got)
	}
	if got["nas.canilho.net/env"] != "lab" {
		t.Errorf("env label not propagated: %v", got)
	}
	if _, ok := got[ManagedByLabel(p)]; ok {
		t.Error("managed-by must not be propagated from the namespace")
	}
}

// A custom --label-prefix must be honoured on both read and filter, otherwise the
// tool only works for whoever picked the default.
func TestPropagatedLabelsHonoursCustomPrefix(t *testing.T) {
	const p = "example.com/"
	got := propagatedLabels(map[string]string{
		ManagedByLabel(p):      "reg",
		ClusterLabel(p):        "c1",
		"example.com/tier":     "prod",
		"nas.canilho.net/flux": "true", // different prefix, must NOT leak
	}, p)
	if len(got) != 1 || got["example.com/tier"] != "prod" {
		t.Fatalf("expected only example.com/tier, got %v", got)
	}
}

// vcluster writes both `vc-<name>` (the kubeconfig) and `vc-config-<name>` (its
// own config) so a `vc-*` glob matches both, and the decoy sorts FIRST. Matching
// on name alone picked the decoy and skipped the namespace entirely.
func TestParseKubeconfigVclusterShape(t *testing.T) {
	server, config, err := parseKubeconfig([]byte(vclusterKubeconfig))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server != "https://192.168.1.194" {
		t.Errorf("server = %q, want the exportKubeConfig.server value", server)
	}
	var got argoClusterConfig
	if err := json.Unmarshal([]byte(config), &got); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	// vcluster 0.36 issues client certificates, not the bearer token older
	// versions used.
	if got.TLSClientConfig.CertData == "" || got.TLSClientConfig.KeyData == "" {
		t.Errorf("expected client certificates, got %+v", got.TLSClientConfig)
	}
}
