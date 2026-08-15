package registrar

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// certDataValidFor builds the base64 PEM ArgoCD stores in certData, for a
// certificate expiring after d. A real certificate rather than a fixture,
// because the whole point is that x509 parsing is exercised.
func certDataValidFor(t *testing.T, d time.Duration) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "system:admin", Organization: []string{"system:masters"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(d),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return base64.StdEncoding.EncodeToString(pemBytes)
}

func configWithCert(t *testing.T, certData string) []byte {
	t.Helper()
	blob, err := json.Marshal(argoClusterConfig{
		TLSClientConfig: argoTLSClientConfig{CertData: certData},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return blob
}

func TestCredentialExpiryBuckets(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		in   time.Duration
		want string
	}{
		"long lived":   {365 * 24 * time.Hour, expiryOK},
		"under 30d":    {20 * 24 * time.Hour, expiryLt30d},
		"under 7d":     {3 * 24 * time.Hour, expiryLt7d},
		"under 24h":    {2 * time.Hour, expiryLt24h},
		"already gone": {-time.Hour, expiryExpired},
	} {
		t.Run(name, func(t *testing.T) {
			got, notAfter, dated := credentialExpiry(
				configWithCert(t, certDataValidFor(t, tc.in)), now)
			if got != tc.want {
				t.Errorf("bucket = %q, want %q", got, tc.want)
			}
			if !dated {
				t.Error("a certificate-backed registration reported no date")
			}
			if notAfter.IsZero() {
				t.Error("notAfter was not reported")
			}
		})
	}
}

// A bearer token has no expiry this can read, and a legacy ServiceAccount token
// has none at all. It must not be counted as healthy.
func TestBearerTokenIsUnmeasuredNotHealthy(t *testing.T) {
	blob, err := json.Marshal(argoClusterConfig{BearerToken: "t"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, _, dated := credentialExpiry(blob, time.Now())
	if got != expiryNone {
		t.Errorf("bucket = %q, want %q", got, expiryNone)
	}
	if dated {
		t.Error("a token registration claimed to carry a date")
	}
}

// A Secret with no config at all is not damaged: the key was never written, or
// was emptied. Reporting it as a corrupt certificate sends an operator hunting
// something that does not exist.
func TestEmptyConfigIsAbsentNotUnreadable(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t"} {
		if got, _, _ := credentialExpiry([]byte(in), time.Now()); got != expiryAbsent {
			t.Errorf("input %q: bucket = %q, want %q", in, got, expiryAbsent)
		}
	}
	if got, _, _ := credentialExpiry([]byte("{not json"), time.Now()); got != expiryUnreadable {
		t.Errorf("malformed config = %q, want %q", got, expiryUnreadable)
	}
}

// Unreadable is distinct from none: something is wrong rather than absent.
func TestUnreadableCertificateIsItsOwnBucket(t *testing.T) {
	for name, certData := range map[string]string{
		"not base64": "!!!!",
		"not pem":    base64.StdEncoding.EncodeToString([]byte("nonsense")),
		"not a cert": base64.StdEncoding.EncodeToString(
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("bad")})),
	} {
		t.Run(name, func(t *testing.T) {
			got, _, _ := credentialExpiry(configWithCert(t, certData), time.Now())
			if got != expiryUnreadable {
				t.Errorf("bucket = %q, want %q", got, expiryUnreadable)
			}
		})
	}
}

// Never quote the input: it came out of a credential-bearing Secret.
func TestCertificateErrorsDoNotQuoteTheirInput(t *testing.T) {
	const secret = "SUPERSECRETCERTDATA"
	_, err := parseCertificate(secret)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); strings.Contains(got, secret) {
		t.Errorf("error quoted its input: %s", got)
	}
}

// The audit is where this is measured, because it sees what ArgoCD is holding
// rather than what the source publishes. Those diverge exactly for a frozen
// registration, which is the failure the metric exists to catch.
func TestAuditBucketsExpiringRegistrations(t *testing.T) {
	expiring := registeredSecret("dying", testNS)
	expiring.Data["config"] = configWithCert(t, certDataValidFor(t, 2*time.Hour))
	healthy := registeredSecret("fine", testNS)
	healthy.Data["config"] = configWithCert(t, certDataValidFor(t, 300*24*time.Hour))

	r, _ := newTestRegistrar(expiring, healthy)
	if err := r.AuditUnrouted(context.Background()); err != nil {
		t.Fatalf("audit: %v", err)
	}

	if got := testutil.ToFloat64(registrations.WithLabelValues(stateActive, expiryLt24h)); got != 1 {
		t.Errorf("credential_expiry=lt_24h = %v, want 1", got)
	}
	if got := testutil.ToFloat64(registrations.WithLabelValues(stateActive, expiryOK)); got != 1 {
		t.Errorf("credential_expiry=ok = %v, want 1", got)
	}
}

// Set every series every pass, or a bucket that empties keeps reporting its last
// value forever and the alert never clears.
func TestEmptiedBucketReturnsToZero(t *testing.T) {
	expiring := registeredSecret("dying", testNS)
	expiring.Data["config"] = configWithCert(t, certDataValidFor(t, 2*time.Hour))

	r, _ := newTestRegistrar(expiring)
	if err := r.AuditUnrouted(context.Background()); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if got := testutil.ToFloat64(registrations.WithLabelValues(stateActive, expiryLt24h)); got != 1 {
		t.Fatalf("setup: lt_24h = %v, want 1", got)
	}

	// The cluster is fixed, or gone. Either way the bucket must empty.
	clean, _ := newTestRegistrar()
	if err := clean.AuditUnrouted(context.Background()); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if got := testutil.ToFloat64(registrations.WithLabelValues(stateActive, expiryLt24h)); got != 0 {
		t.Errorf("lt_24h stayed at %v after the population emptied", got)
	}
}
