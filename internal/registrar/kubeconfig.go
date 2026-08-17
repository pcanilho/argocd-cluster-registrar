// Package registrar turns child-cluster kubeconfig Secrets into ArgoCD cluster
// Secrets, and removes the ones whose cluster is gone.
package registrar

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// kubeconfig is the subset of a kubeconfig this tool needs. Deliberately a
// hand-rolled struct rather than clientcmd: we never build a REST client from
// it, we only re-shape its credentials into ArgoCD's own format.
type kubeconfigCluster struct {
	Server                   string `yaml:"server"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
	CertificateAuthority     string `yaml:"certificate-authority"`
	InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
	// TLSServerName is what the certificate must be valid for when it does not
	// match the address in Server. Dropping it produced a registration with the
	// right CA and the right credentials that still failed hostname verification.
	TLSServerName string `yaml:"tls-server-name"`
	// ProxyURL is carried rather than dropped: same failure shape as
	// TLSServerName, a registration that looks healthy and fails at connect time.
	ProxyURL string `yaml:"proxy-url"`
}

type kubeconfigUser struct {
	Token                 string          `yaml:"token"`
	TokenFile             string          `yaml:"tokenFile"`
	ClientCertificateData string          `yaml:"client-certificate-data"`
	ClientKeyData         string          `yaml:"client-key-data"`
	ClientCertificate     string          `yaml:"client-certificate"`
	ClientKey             string          `yaml:"client-key"`
	Exec                  *kubeconfigExec `yaml:"exec"`
	AuthProvider          any             `yaml:"auth-provider"`
}

type kubeconfig struct {
	Clusters []struct {
		Name    string            `yaml:"name"`
		Cluster kubeconfigCluster `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string         `yaml:"name"`
		User kubeconfigUser `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	CurrentContext string `yaml:"current-context"`
}

// resolve picks the cluster/user pair the kubeconfig actually points at.
//
// Taking Clusters[0] and Users[0] is wrong on any multi-entry kubeconfig, which
// CAPI-style providers do emit. It can pair one cluster's server with another
// cluster's credentials, which does not merely misconfigure ArgoCD: it makes
// ArgoCD present cluster A's token to cluster B on every connection, handing B a
// replayable credential for A. Across tenants that is a credential leak, so this
// refuses to guess.
func (kc *kubeconfig) resolve() (kubeconfigCluster, kubeconfigUser, error) {
	var zc kubeconfigCluster
	var zu kubeconfigUser

	if len(kc.Clusters) == 0 {
		return zc, zu, fmt.Errorf("kubeconfig contains no clusters")
	}
	if len(kc.Users) == 0 {
		return zc, zu, fmt.Errorf("kubeconfig contains no users")
	}

	// Unambiguous: exactly one of each, so context resolution cannot change the
	// answer. This is what k3k and vcluster emit.
	if len(kc.Clusters) == 1 && len(kc.Users) == 1 {
		return kc.Clusters[0].Cluster, kc.Users[0].User, nil
	}

	if kc.CurrentContext == "" {
		return zc, zu, fmt.Errorf(
			"kubeconfig has %d clusters and %d users but no current-context; refusing to guess",
			len(kc.Clusters), len(kc.Users))
	}
	for _, ctx := range kc.Contexts {
		if ctx.Name != kc.CurrentContext {
			continue
		}
		var (
			cluster *kubeconfigCluster
			user    *kubeconfigUser
		)
		for i := range kc.Clusters {
			if kc.Clusters[i].Name == ctx.Context.Cluster {
				cluster = &kc.Clusters[i].Cluster
				break
			}
		}
		for i := range kc.Users {
			if kc.Users[i].Name == ctx.Context.User {
				user = &kc.Users[i].User
				break
			}
		}
		if cluster == nil {
			return zc, zu, fmt.Errorf("current-context %q references unknown cluster %q",
				kc.CurrentContext, ctx.Context.Cluster)
		}
		if user == nil {
			return zc, zu, fmt.Errorf("current-context %q references unknown user %q",
				kc.CurrentContext, ctx.Context.User)
		}
		return *cluster, *user, nil
	}
	return zc, zu, fmt.Errorf("current-context %q has no matching context entry", kc.CurrentContext)
}

// argoTLSClientConfig mirrors ArgoCD's cluster Secret `config.tlsClientConfig`.
type argoTLSClientConfig struct {
	Insecure   bool   `json:"insecure"`
	CaData     string `json:"caData,omitempty"`
	CertData   string `json:"certData,omitempty"`
	KeyData    string `json:"keyData,omitempty"`
	ServerName string `json:"serverName,omitempty"`
}

// argoClusterConfig mirrors ArgoCD's cluster Secret `config` blob. ProxyURL is
// top-level, a sibling of tlsClientConfig rather than inside it.
type argoClusterConfig struct {
	BearerToken        string                  `json:"bearerToken,omitempty"`
	TLSClientConfig    argoTLSClientConfig     `json:"tlsClientConfig"`
	ProxyURL           string                  `json:"proxyUrl,omitempty"`
	AWSAuthConfig      *argoAWSAuthConfig      `json:"awsAuthConfig,omitempty"`
	ExecProviderConfig *argoExecProviderConfig `json:"execProviderConfig,omitempty"`
}

// parsedKubeconfig is what one candidate yielded. Insecure is reported so the
// caller can warn: it disables the CA pinning the rest of this design leans on.
type parsedKubeconfig struct {
	server   string
	config   string
	insecure bool
	// execCommand is the source command that was translated, empty when the
	// credential was copied. Nothing in a translated config is a credential, so
	// how the registration authenticates is otherwise invisible.
	execCommand string
}

// inClusterServer is the one server value ArgoCD treats as magic: it calls
// rest.InClusterConfig() and ignores caData, certData and keyData entirely, so a
// child cluster registered under it silently points at the management cluster.
const inClusterServer = "https://kubernetes.default.svc"

// inClusterHosts are the spellings that reach the same place as
// inClusterServer. An exact compare missed the port and the FQDN forms.
var inClusterHosts = map[string]bool{
	"kubernetes.default.svc":                   true,
	"kubernetes.default.svc:443":               true,
	"kubernetes.default":                       true,
	"kubernetes.default:443":                   true,
	"kubernetes.default.svc.cluster.local":     true,
	"kubernetes.default.svc.cluster.local:443": true,
}

// isInClusterServer reports whether a server URL resolves to the apiserver
// ArgoCD would reach through rest.InClusterConfig().
func isInClusterServer(server string) bool {
	u, err := url.Parse(server)
	if err != nil {
		return false
	}
	return inClusterHosts[strings.ToLower(u.Host)]
}

// proxySchemes are what ArgoCD's own ParseProxyUrl accepts.
var proxySchemes = map[string]bool{
	"http": true, "https": true, "socks5": true, "socks5h": true,
}

// parseKubeconfig extracts the server URL and an ArgoCD-shaped credential blob
// from a raw kubeconfig.
//
// Both auth styles are handled because the runtime is deliberately swappable:
// k3k and plain k3s issue CLIENT CERTIFICATES (CN system:admin), while vcluster
// issued a bearer token. Whichever is present wins; certs take precedence.
func parseKubeconfig(raw []byte, allowExec bool) (parsedKubeconfig, error) {
	var zero parsedKubeconfig

	var kc kubeconfig
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return zero, fmt.Errorf("parse kubeconfig: %w", err)
	}

	c, u, err := kc.resolve()
	if err != nil {
		return zero, err
	}

	if c.Server == "" {
		return zero, fmt.Errorf("kubeconfig cluster has no server URL")
	}
	if parsed, perr := url.Parse(c.Server); perr != nil || parsed.Host == "" {
		return zero, fmt.Errorf("kubeconfig server %q is not a usable URL", c.Server)
	}
	// Normalised the way ArgoCD normalises it, and stored normalised. Its cluster
	// indexer keys on strings.TrimRight(server, "/") and GetClusterByURL trims the
	// same way, so `https://x/` and `https://x` are one cluster to ArgoCD. Storing
	// the raw form would let a trailing slash walk past the collision check.
	server := strings.TrimRight(c.Server, "/")

	if isInClusterServer(server) {
		return zero, fmt.Errorf(
			"kubeconfig server is %s, which ArgoCD resolves to its OWN in-cluster config, "+
				"ignoring the CA and credentials here; the child cluster must be reachable "+
				"by an address of its own", inClusterServer)
	}

	proxy, err := parseProxyURL(c.ProxyURL, c.InsecureSkipTLSVerify)
	if err != nil {
		return zero, err
	}

	// Only embedded credentials can be carried into an ArgoCD Secret. The file
	// and exec forms would otherwise fall through to the misleading "no
	// credentials" error below, or worse, produce an empty caData with
	// insecure:false, which fails x509 verification with no clue as to why.
	if c.CertificateAuthority != "" {
		return zero, fmt.Errorf("kubeconfig references its CA by file path (%q); only certificate-authority-data is supported", c.CertificateAuthority)
	}
	if u.ClientCertificate != "" || u.ClientKey != "" || u.TokenFile != "" {
		return zero, fmt.Errorf("kubeconfig references credentials by file path; only the embedded *-data forms are supported")
	}
	// auth-provider stays refused outright: client-go removed the in-tree gcp and
	// azure providers in 1.26, so there is nothing on the other side to map to.
	if u.AuthProvider != nil {
		return zero, fmt.Errorf("kubeconfig uses an auth-provider credential plugin, which has no ArgoCD equivalent")
	}

	cfg := argoClusterConfig{
		TLSClientConfig: argoTLSClientConfig{
			Insecure:   c.InsecureSkipTLSVerify,
			CaData:     c.CertificateAuthorityData,
			ServerName: c.TLSServerName,
		},
		ProxyURL: proxy,
	}

	// Exec is tried LAST. A static credential needs no ambient identity, so where
	// a kubeconfig somehow carries both it is the safer answer -- and ArgoCD
	// ignores bearerToken whenever either exec field is set, so emitting both
	// would put a credential in the Secret that is never used.
	var execCommand string
	switch {
	case u.ClientCertificateData != "" && u.ClientKeyData != "":
		cfg.TLSClientConfig.CertData = u.ClientCertificateData
		cfg.TLSClientConfig.KeyData = u.ClientKeyData
	case u.Token != "":
		cfg.BearerToken = u.Token
	case u.Exec != nil:
		if !allowExec {
			return zero, fmt.Errorf(
				"kubeconfig uses a %q exec credential plugin; translating one is off for this "+
					"provider, so nothing was copied. Enable --exec-credentials and use a "+
					"provider that allows it",
				path.Base(u.Exec.Command))
		}
		cred, terr := translateExec(u.Exec)
		if terr != nil {
			return zero, terr
		}
		cfg.AWSAuthConfig, cfg.ExecProviderConfig = cred.aws, cred.exec
		execCommand = path.Base(u.Exec.Command)
	default:
		return zero, fmt.Errorf("kubeconfig user has neither client certificates, a token, nor an exec credential")
	}

	// gosec G117 flags marshalling a struct with a credential-shaped field. That
	// is precisely the job here: ArgoCD's cluster Secret format carries the
	// bearer token or client key in this blob, and the result goes straight into
	// a Secret. Nothing is logged or returned to a caller that would leak it.
	blob, err := json.Marshal(cfg) // #nosec G117 -- credentials are the payload, by design
	if err != nil {
		return zero, fmt.Errorf("marshal argocd cluster config: %w", err)
	}
	return parsedKubeconfig{
		server:      server,
		config:      string(blob),
		insecure:    c.InsecureSkipTLSVerify,
		execCommand: execCommand,
	}, nil
}

// parseProxyURL validates a kubeconfig proxy-url for ArgoCD.
//
// Refused with insecure-skip-tls-verify because that pair is the only shape
// where the proxy can read the credential rather than just relay it, and it has
// no legitimate use.
func parseProxyURL(raw string, insecure bool) (string, error) {
	if raw == "" {
		return "", nil
	}
	p, err := url.Parse(raw)
	if err != nil || p.Host == "" {
		return "", fmt.Errorf("kubeconfig proxy-url %q is not a usable URL", raw)
	}
	if !proxySchemes[p.Scheme] {
		return "", fmt.Errorf(
			"kubeconfig proxy-url %q uses scheme %q; ArgoCD accepts only http, https and socks5",
			raw, p.Scheme)
	}
	if p.User != nil {
		return "", fmt.Errorf(
			"kubeconfig proxy-url embeds credentials; they would be copied into the ArgoCD " +
				"namespace, so set them on the proxy or drop them")
	}
	if insecure {
		return "", fmt.Errorf(
			"kubeconfig sets both proxy-url and insecure-skip-tls-verify; that combination " +
				"lets the proxy read the cluster credential rather than relay it")
	}
	return p.String(), nil
}
