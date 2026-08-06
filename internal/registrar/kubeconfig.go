// Package registrar turns child-cluster kubeconfig Secrets into ArgoCD cluster
// Secrets, and removes the ones whose cluster is gone.
package registrar

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// kubeconfig is the subset of a kubeconfig this tool needs. Deliberately a
// hand-rolled struct rather than clientcmd: we never build a REST client from
// it, we only re-shape its credentials into ArgoCD's own format.
type kubeconfig struct {
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string `yaml:"token"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKeyData         string `yaml:"client-key-data"`
		} `yaml:"user"`
	} `yaml:"users"`
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
	if len(kc.Clusters) == 0 {
		return "", "", fmt.Errorf("kubeconfig contains no clusters")
	}
	if len(kc.Users) == 0 {
		return "", "", fmt.Errorf("kubeconfig contains no users")
	}

	c := kc.Clusters[0].Cluster
	u := kc.Users[0].User

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

	blob, err := json.Marshal(cfg)
	if err != nil {
		return "", "", fmt.Errorf("marshal argocd cluster config: %w", err)
	}
	return c.Server, string(blob), nil
}
