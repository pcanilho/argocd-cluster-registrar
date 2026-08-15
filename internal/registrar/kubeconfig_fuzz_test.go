package registrar

import (
	"strings"
	"testing"
)

// The kubeconfig comes out of a Secret in a tenant namespace, so its bytes are
// attacker-influenced. parseKubeconfig may reject anything it likes; it may not
// panic, and it may not return a usable result it has not validated.
func FuzzParseKubeconfig(f *testing.F) {
	f.Add([]byte(""), true)
	f.Add([]byte("apiVersion: v1\n"), true)
	f.Add([]byte(execKubeconfigWith("aws-iam-authenticator", "token", "-i", "c")), true)
	f.Add([]byte(execKubeconfigWith("kubelogin", "get-token", "--server-id", "s")), false)
	f.Add([]byte("apiVersion: v1\nclusters:\n- name: c\n  cluster:\n"+
		"    server: https://a\n    certificate-authority-data: Y2E=\n"+
		"users:\n- name: u\n  user:\n    token: t\n"), true)
	f.Add([]byte("clusters:\n- cluster:\n    server: https://a/\n"+
		"    insecure-skip-tls-verify: true\n    proxy-url: socks5h://p:1\n"), true)

	f.Fuzz(func(t *testing.T, raw []byte, allowExec bool) {
		pk, err := parseKubeconfig(raw, allowExec)
		if err != nil {
			return
		}

		// A successful parse is a promise the caller acts on, so the invariants
		// it relies on have to hold for every input that reaches this point.
		if pk.server == "" {
			t.Fatal("parsed with an empty server")
		}
		if strings.HasSuffix(pk.server, "/") {
			t.Fatalf("server was not normalised: %q", pk.server)
		}
		if isInClusterServer(pk.server) {
			t.Fatalf("in-cluster address survived the guard: %q", pk.server)
		}
		if pk.config == "" {
			t.Fatal("parsed with an empty config blob")
		}
		if !allowExec && pk.execCommand != "" {
			t.Fatalf("exec credential returned with translation off: %q", pk.execCommand)
		}
	})
}
