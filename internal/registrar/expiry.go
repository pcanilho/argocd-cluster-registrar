package registrar

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"
)

// credentialExpiry classifies how long the credential in an ArgoCD cluster
// Secret has left, as one of the expiry buckets.
//
// It reads the written Secret, not the source kubeconfig: the two diverge
// exactly for a registration that has stopped being refreshed, which is the
// failure this measures.
//
// Only the certificate is decoded, never the key. CAPI and kubeadm issue client
// certificates for 365 days by default, so this is a real clock.
func credentialExpiry(config []byte, now time.Time) (bucket string, notAfter time.Time, ok bool) {
	// Absent is not corrupt: no config yet, versus a config that will not parse.
	if len(bytes.TrimSpace(config)) == 0 {
		return expiryAbsent, time.Time{}, false
	}

	var cfg argoClusterConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return expiryUnreadable, time.Time{}, false
	}
	if cfg.TLSClientConfig.CertData == "" {
		// A bearer token or an exec credential. Neither carries an expiry we can
		// read, and a JWT `exp` is not one either: a legacy ServiceAccount token
		// has none at all.
		return expiryNone, time.Time{}, false
	}

	cert, err := parseCertificate(cfg.TLSClientConfig.CertData)
	if err != nil {
		return expiryUnreadable, time.Time{}, false
	}

	left := cert.NotAfter.Sub(now)
	switch {
	case left <= 0:
		return expiryExpired, cert.NotAfter, true
	case left < 24*time.Hour:
		return expiryLt24h, cert.NotAfter, true
	case left < 7*24*time.Hour:
		return expiryLt7d, cert.NotAfter, true
	case left < 30*24*time.Hour:
		return expiryLt30d, cert.NotAfter, true
	default:
		return expiryOK, cert.NotAfter, true
	}
}

// parseCertificate decodes the base64 PEM ArgoCD stores in certData.
//
// The error deliberately never quotes its input. Everything reaching here came
// out of a credential-bearing Secret, and this one is the public half only
// because certData and keyData sit side by side in the same blob.
func parseCertificate(certData string) (*x509.Certificate, error) {
	der, err := base64.StdEncoding.DecodeString(certData)
	if err != nil {
		return nil, fmt.Errorf("certData is not valid base64")
	}
	block, _ := pem.Decode(der)
	if block == nil {
		return nil, fmt.Errorf("certData is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certData is not a usable certificate")
	}
	return cert, nil
}
