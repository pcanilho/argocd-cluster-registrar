package registrar

import (
	"encoding/json"
	"strings"
	"testing"
)

// Shape taken verbatim from a live k3k v1.2.0-rc3 `k3k-<name>-kubeconfig`
// Secret: client certificates, and a server URL that is the LoadBalancer address
// on port 443 (not the in-cluster Service DNS name on 6443).
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

// Kamaji's standalone `<tcp>-admin-kubeconfig` Secret, key `admin.conf`. kubeadm
// shapes it, so the names are the kubeadm ones rather than the cluster's.
const kamajiKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: tenant-00
  cluster:
    server: https://192.168.1.195:6443
    certificate-authority-data: Y2FkYXRh
users:
- name: kubernetes-admin
  user:
    client-certificate-data: Y2VydGRhdGE=
    client-key-data: a2V5ZGF0YQ==
contexts:
- name: kubernetes-admin@tenant-00
  context:
    cluster: tenant-00
    user: kubernetes-admin
current-context: kubernetes-admin@tenant-00
`

// Kamaji's OTHER key, `admin.svc`, written alongside admin.conf when the control
// plane advertises an in-cluster Service address. Deliberately a different server
// from kamajiKubeconfig: a fallthrough test whose two candidates resolve to the
// same address passes whichever one was used, which proves nothing.
const kamajiSvcKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: tenant-00
  cluster:
    server: https://tenant-00.kamaji.svc:6443
    certificate-authority-data: Y2FkYXRh
users:
- name: kubernetes-admin
  user:
    client-certificate-data: Y2VydGRhdGE=
    client-key-data: a2V5ZGF0YQ==
contexts:
- name: kubernetes-admin@tenant-00
  context:
    cluster: tenant-00
    user: kubernetes-admin
current-context: kubernetes-admin@tenant-00
`

// The Cluster API contract shape: Secret `<cluster>-kubeconfig`, key `value`.
// CAPI emits multiple entries with an explicit current-context, which is exactly
// the case resolve() refuses to guess about.
const capiKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: capi-child
  cluster:
    server: https://192.168.1.196:6443
    certificate-authority-data: Y2FkYXRh
users:
- name: capi-child-admin
  user:
    client-certificate-data: Y2VydGRhdGE=
    client-key-data: a2V5ZGF0YQ==
contexts:
- name: capi-child-admin@capi-child
  context:
    cluster: capi-child
    user: capi-child-admin
current-context: capi-child-admin@capi-child
`

// CAPA's EKS path writes a SECOND `<cluster>-user-kubeconfig` carrying an exec
// credential. Shape taken from cluster-api-provider-aws
// pkg/cloud/services/eks/config.go, whose default TokenMethod is
// iam-authenticator, so `aws-iam-authenticator token -i <eksClusterName>`.
//
// The args matter and must not be dropped from this fixture. Without them a
// translation attempt would fail for lack of a cluster name rather than because
// the gate is shut, and the refusal test would keep passing with the gate open.
const execKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: managed
  cluster:
    server: https://example.eks.amazonaws.com
    certificate-authority-data: Y2FkYXRh
users:
- name: managed
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws-iam-authenticator
      args:
      - token
      - -i
      - my-eks-cluster
current-context: managed
contexts:
- name: managed
  context:
    cluster: managed
    user: managed
`

func TestParseKubeconfigKamajiShape(t *testing.T) {
	pk, err := parseKubeconfig([]byte(kamajiKubeconfig), false)
	server := pk.server
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server != "https://192.168.1.195:6443" {
		t.Errorf("server = %q", server)
	}
}

func TestParseKubeconfigCAPIShape(t *testing.T) {
	pk, err := parseKubeconfig([]byte(capiKubeconfig), false)
	server, config := pk.server, pk.config
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server != "https://192.168.1.196:6443" {
		t.Errorf("server = %q", server)
	}
	if !strings.Contains(config, "certData") {
		t.Errorf("expected client certs in the config blob, got %s", config)
	}
}

func TestParseKubeconfigClientCerts(t *testing.T) {
	pk, err := parseKubeconfig([]byte(k3kKubeconfig), false)
	server, config := pk.server, pk.config
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server != k3kServer {
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
	pk, err := parseKubeconfig([]byte(tokenKubeconfig), false)
	server, config := pk.server, pk.config
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
			if _, err := parseKubeconfig([]byte(in), false); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestPropagatedLabels(t *testing.T) {
	const p = "nas.canilho.net/"
	got := propagatedLabels(map[string]string{
		ManagedByLabel(p):             testManagedBy, // describes the source
		ClusterLabel(p):               "sandbox",     // set explicitly by apply()
		"nas.canilho.net/flux":        "true",        // must reach the cluster Secret
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

// The source namespace is tenant-controlled and apply() copies these labels OVER
// the ones it just computed, so anything not withheld here can be spoofed.
//
// `source-namespace` is the sharp one: it is the pointer garbage collection
// follows to prove a source is gone. A namespace claiming
// `<prefix>source-namespace: kube-system` would aim that proof at a namespace
// that never disappears, and its registration could then never be collected.
func TestPropagatedLabelsWithholdsReservedLabels(t *testing.T) {
	const p = "example.com/"
	got := propagatedLabels(map[string]string{
		ManagedByLabel(p):        "reg",
		ClusterLabel(p):          "c1",
		SourceNamespaceLabel(p):  "kube-system",
		ProviderLabel(p):         "not-really",
		"example.com/legitimate": "yes",
	}, p)

	for _, reserved := range reservedSuffixes {
		if _, ok := got[p+reserved]; ok {
			t.Errorf("%q was propagated from the namespace and can therefore be spoofed", p+reserved)
		}
	}
	if got["example.com/legitimate"] != "yes" {
		t.Errorf("ordinary prefixed labels must still propagate, got %v", got)
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
		"nas.canilho.net/flux": "true", // different prefix, must not leak
	}, p)
	if len(got) != 1 || got["example.com/tier"] != "prod" {
		t.Fatalf("expected only example.com/tier, got %v", got)
	}
}

// vcluster writes both `vc-<name>` (the kubeconfig) and `vc-config-<name>` (its
// own config) so a `vc-*` glob matches both. Matching on name alone picked the
// decoy and skipped the namespace entirely.
func TestParseKubeconfigVclusterShape(t *testing.T) {
	pk, err := parseKubeconfig([]byte(vclusterKubeconfig), false)
	server, config := pk.server, pk.config
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

// Taking Clusters[0]/Users[0] would pair prod's server with sandbox's
// credentials, or vice versa. That is not just a misconfiguration: ArgoCD would
// then present one cluster's token to another cluster on every connection,
// handing it a replayable credential. current-context must decide.
const multiContextKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: prod
  cluster: {server: https://prod.example.com, certificate-authority-data: cHJvZGNh}
- name: sandbox
  cluster: {server: https://sandbox.example.com, certificate-authority-data: c2JjYQ==}
users:
- name: prod-admin
  user: {token: prod-token}
- name: sandbox-admin
  user: {token: sandbox-token}
contexts:
- name: prod
  context: {cluster: prod, user: prod-admin}
- name: sandbox
  context: {cluster: sandbox, user: sandbox-admin}
current-context: sandbox
`

func TestParseKubeconfigHonoursCurrentContext(t *testing.T) {
	pk, err := parseKubeconfig([]byte(multiContextKubeconfig), false)
	server, config := pk.server, pk.config
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if server != "https://sandbox.example.com" {
		t.Errorf("server = %q, want the current-context cluster, not Clusters[0]", server)
	}
	var got argoClusterConfig
	if err := json.Unmarshal([]byte(config), &got); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if got.BearerToken != "sandbox-token" {
		t.Errorf("bearerToken = %q; pairing a cluster with another cluster's credential leaks it", got.BearerToken)
	}
	if got.TLSClientConfig.CaData != "c2JjYQ==" {
		t.Errorf("caData = %q, want sandbox's CA", got.TLSClientConfig.CaData)
	}
}

func TestParseKubeconfigRefusesToGuessWhenAmbiguous(t *testing.T) {
	noCurrent := `apiVersion: v1
kind: Config
clusters:
- name: a
  cluster: {server: https://a}
- name: b
  cluster: {server: https://b}
users:
- name: ua
  user: {token: ta}
- name: ub
  user: {token: tb}
`
	if _, err := parseKubeconfig([]byte(noCurrent), false); err == nil {
		t.Error("expected a refusal on an ambiguous multi-entry kubeconfig, got nil")
	}
}

func TestParseKubeconfigRejectsUnusableCredentials(t *testing.T) {
	for name, in := range map[string]string{
		"empty server": "apiVersion: v1\nclusters:\n- name: a\n  cluster: {server: \"\"}\nusers:\n- name: a\n  user: {token: t}\n",
		"bad url":      "apiVersion: v1\nclusters:\n- name: a\n  cluster: {server: \"not a url\"}\nusers:\n- name: a\n  user: {token: t}\n",
		"ca by file":   "apiVersion: v1\nclusters:\n- name: a\n  cluster: {server: https://a, certificate-authority: /etc/ca.crt}\nusers:\n- name: a\n  user: {token: t}\n",
		"cert by file": "apiVersion: v1\nclusters:\n- name: a\n  cluster: {server: https://a}\nusers:\n- name: a\n  user: {client-certificate: /etc/c.crt, client-key: /etc/c.key}\n",
		"exec plugin":  "apiVersion: v1\nclusters:\n- name: a\n  cluster: {server: https://a}\nusers:\n- name: a\n  user:\n    exec: {command: aws}\n",
		"unknown ctx":  "apiVersion: v1\nclusters:\n- name: a\n  cluster: {server: https://a}\n- name: b\n  cluster: {server: https://b}\nusers:\n- name: ua\n  user: {token: t}\n- name: ub\n  user: {token: u}\ncontexts:\n- name: c\n  context: {cluster: zzz, user: ua}\ncurrent-context: c\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseKubeconfig([]byte(in), false); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

// ArgoCD resolves this one server value through rest.InClusterConfig() and
// ignores caData/certData/keyData entirely, so a child registered under it
// silently points at the management cluster and looks healthy doing it.
func TestRefusesInClusterServerAddress(t *testing.T) {
	for _, srv := range []string{
		"https://kubernetes.default.svc",
		"https://kubernetes.default.svc/",
	} {
		kc := "apiVersion: v1\nclusters:\n- name: c\n  cluster:\n    server: " + srv +
			"\n    certificate-authority-data: Y2E=\nusers:\n- name: u\n  user:\n    token: t\n"
		if _, err := parseKubeconfig([]byte(kc), false); err == nil {
			t.Errorf("server %q was accepted", srv)
		}
	}
}

func TestCarriesProxyURL(t *testing.T) {
	kc := "apiVersion: v1\nclusters:\n- name: c\n  cluster:\n    server: https://a\n" +
		"    certificate-authority-data: Y2E=\n    proxy-url: http://proxy.internal:3128\n" +
		"users:\n- name: u\n  user:\n    token: t\n"
	pk, err := parseKubeconfig([]byte(kc), false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got argoClusterConfig
	if err := json.Unmarshal([]byte(pk.config), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ProxyURL != "http://proxy.internal:3128" {
		t.Errorf("proxyUrl = %q", got.ProxyURL)
	}
}

// Absent proxy-url must not emit an empty field.
func TestNoProxyURLEmitsNoProxyURL(t *testing.T) {
	pk, err := parseKubeconfig([]byte(k3kKubeconfig), false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Contains(pk.config, "proxyUrl") {
		t.Errorf("config carries an empty proxyUrl: %s", pk.config)
	}
}

func TestRefusesUnusableProxyURLs(t *testing.T) {
	cases := map[string]string{
		"bad scheme":    "    proxy-url: ftp://p:21\n",
		"embeds creds":  "    proxy-url: http://user:pass@p:3128\n",
		"not a url":     "    proxy-url: \"::\"\n",
		"with insecure": "    proxy-url: http://p:3128\n    insecure-skip-tls-verify: true\n",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			kc := "apiVersion: v1\nclusters:\n- name: c\n  cluster:\n    server: https://a\n" +
				"    certificate-authority-data: Y2E=\n" + line +
				"users:\n- name: u\n  user:\n    token: t\n"
			if _, err := parseKubeconfig([]byte(kc), false); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// insecure is reported so the caller can warn; it stays copied through.
func TestInsecureIsReportedToTheCaller(t *testing.T) {
	kc := "apiVersion: v1\nclusters:\n- name: c\n  cluster:\n    server: https://a\n" +
		"    insecure-skip-tls-verify: true\nusers:\n- name: u\n  user:\n    token: t\n"
	pk, err := parseKubeconfig([]byte(kc), false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !pk.insecure {
		t.Error("insecure was not reported")
	}
}

// An exact string compare missed the port and FQDN spellings, all of which reach
// the same apiserver ArgoCD would use through rest.InClusterConfig().
func TestInClusterServerIsRefusedInEverySpelling(t *testing.T) {
	for _, srv := range []string{
		"https://kubernetes.default.svc",
		"https://kubernetes.default.svc/",
		"https://kubernetes.default.svc:443",
		"https://kubernetes.default",
		"https://kubernetes.default:443",
		"https://kubernetes.default.svc.cluster.local",
		"https://kubernetes.default.svc.cluster.local:443",
		"https://KUBERNETES.DEFAULT.SVC",
	} {
		kc := "apiVersion: v1\nclusters:\n- name: c\n  cluster:\n    server: " + srv +
			"\n    certificate-authority-data: Y2E=\nusers:\n- name: u\n  user:\n    token: t\n"
		if _, err := parseKubeconfig([]byte(kc), false); err == nil {
			t.Errorf("server %q was accepted", srv)
		}
	}
	// A real cluster that merely looks similar must still register.
	kc := "apiVersion: v1\nclusters:\n- name: c\n  cluster:\n" +
		"    server: https://kubernetes.default.example.com\n    certificate-authority-data: Y2E=\n" +
		"users:\n- name: u\n  user:\n    token: t\n"
	if _, err := parseKubeconfig([]byte(kc), false); err != nil {
		t.Errorf("a real cluster with a similar name was refused: %v", err)
	}
}

// socks5h is a common spelling and routes DNS through the proxy, which is the
// point of using one. Refusing it turned a working registration into a failure.
func TestProxySchemeSocks5hIsAccepted(t *testing.T) {
	kc := "apiVersion: v1\nclusters:\n- name: c\n  cluster:\n    server: https://a\n" +
		"    certificate-authority-data: Y2E=\n    proxy-url: socks5h://p:1080\n" +
		"users:\n- name: u\n  user:\n    token: t\n"
	pk, err := parseKubeconfig([]byte(kc), false)
	if err != nil {
		t.Fatalf("socks5h was refused: %v", err)
	}
	if !strings.Contains(pk.config, "socks5h://p:1080") {
		t.Errorf("proxyUrl was not carried: %s", pk.config)
	}
}
