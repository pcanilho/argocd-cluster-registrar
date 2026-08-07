// Package registrar turns child-cluster kubeconfig Secrets into ArgoCD cluster
// Secrets, and removes the ones whose cluster is gone.
package registrar

import (
	"encoding/json"
	"fmt"
	"net/url"

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
}

type kubeconfigUser struct {
	Token                 string `yaml:"token"`
	TokenFile             string `yaml:"tokenFile"`
	ClientCertificateData string `yaml:"client-certificate-data"`
	ClientKeyData         string `yaml:"client-key-data"`
	ClientCertificate     string `yaml:"client-certificate"`
	ClientKey             string `yaml:"client-key"`
	Exec                  any    `yaml:"exec"`
	AuthProvider          any    `yaml:"auth-provider"`
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

// argoClusterConfig mirrors ArgoCD's cluster Secret `config` blob.
type argoClusterConfig struct {
	BearerToken     string              `json:"bearerToken,omitempty"`
	TLSClientConfig argoTLSClientConfig `json:"tlsClientConfig"`
}

// parseKubeconfig extracts the server URL and an ArgoCD-shaped credential blob
// from a raw kubeconfig.
//
// Both auth styles are handled because the runtime is deliberately swappable:
// k3k and plain k3s issue CLIENT CERTIFICATES (CN system:admin), while vcluster
// issued a bearer token. Whichever is present wins; certs take precedence.
func parseKubeconfig(raw []byte) (server string, config string, err error) {
	var kc kubeconfig
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return "", "", fmt.Errorf("parse kubeconfig: %w", err)
	}

	c, u, err := kc.resolve()
	if err != nil {
		return "", "", err
	}

	if c.Server == "" {
		return "", "", fmt.Errorf("kubeconfig cluster has no server URL")
	}
	if parsed, perr := url.Parse(c.Server); perr != nil || parsed.Host == "" {
		return "", "", fmt.Errorf("kubeconfig server %q is not a usable URL", c.Server)
	}

	// Only embedded credentials can be carried into an ArgoCD Secret. The file
	// and exec forms would otherwise fall through to the misleading "no
	// credentials" error below, or worse, produce an empty caData with
	// insecure:false, which fails x509 verification with no clue as to why.
	if c.CertificateAuthority != "" {
		return "", "", fmt.Errorf("kubeconfig references its CA by file path (%q); only certificate-authority-data is supported", c.CertificateAuthority)
	}
	if u.ClientCertificate != "" || u.ClientKey != "" || u.TokenFile != "" {
		return "", "", fmt.Errorf("kubeconfig references credentials by file path; only the embedded *-data forms are supported")
	}
	if u.Exec != nil || u.AuthProvider != nil {
		return "", "", fmt.Errorf("kubeconfig uses an exec or auth-provider credential plugin, which cannot be copied into an ArgoCD cluster Secret")
	}

	cfg := argoClusterConfig{
		TLSClientConfig: argoTLSClientConfig{
			Insecure: c.InsecureSkipTLSVerify,
			CaData:   c.CertificateAuthorityData,
		},
	}

	switch {
	case u.ClientCertificateData != "" && u.ClientKeyData != "":
		cfg.TLSClientConfig.CertData = u.ClientCertificateData
		cfg.TLSClientConfig.KeyData = u.ClientKeyData
	case u.Token != "":
		cfg.BearerToken = u.Token
	default:
		return "", "", fmt.Errorf("kubeconfig user has neither client certificates nor a token")
	}

	// gosec G117 flags marshalling a struct with a credential-shaped field. That
	// is precisely the job here: ArgoCD's cluster Secret format carries the
	// bearer token or client key in this blob, and the result goes straight into
	// a Secret. Nothing is logged or returned to a caller that would leak it.
	blob, err := json.Marshal(cfg) // #nosec G117 -- credentials are the payload, by design
	if err != nil {
		return "", "", fmt.Errorf("marshal argocd cluster config: %w", err)
	}
	return c.Server, string(blob), nil
}
