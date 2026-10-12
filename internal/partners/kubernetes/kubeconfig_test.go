package kubernetes

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKubeconfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func TestFileCredentialsResolvesContextClusterAndUser(t *testing.T) {
	ca := base64.StdEncoding.EncodeToString([]byte("not-really-a-certificate"))
	path := writeKubeconfig(t, `
apiVersion: v1
kind: Config
current-context: nightshift
clusters:
  - name: tenant-cluster
    cluster:
      server: https://192.168.203.2:6443
      certificate-authority-data: `+ca+`
contexts:
  - name: nightshift
    context:
      cluster: tenant-cluster
      user: nightshift
      namespace: nightshift
users:
  - name: nightshift
    user:
      token: sa-token-value
`)
	creds, err := loadCredentials(path, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if creds.server != "https://192.168.203.2:6443" {
		t.Fatalf("server = %q", creds.server)
	}
	if creds.namespace != "nightshift" {
		t.Fatalf("namespace = %q", creds.namespace)
	}
	if creds.token != "sa-token-value" {
		t.Fatalf("token = %q", creds.token)
	}
	if string(creds.caPEM) != "not-really-a-certificate" {
		t.Fatalf("ca = %q", creds.caPEM)
	}
}

func TestFileCredentialsHonoursAnExplicitContext(t *testing.T) {
	path := writeKubeconfig(t, `
apiVersion: v1
kind: Config
current-context: first
clusters:
  - name: a
    cluster:
      server: https://a.example:6443
  - name: b
    cluster:
      server: https://b.example:6443
contexts:
  - name: first
    context: {cluster: a, user: u}
  - name: second
    context: {cluster: b, user: u, namespace: nightshift}
users:
  - name: u
    user: {token: t}
`)
	creds, err := loadCredentials(path, "second")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if creds.server != "https://b.example:6443" || creds.namespace != "nightshift" {
		t.Fatalf("creds = %+v", creds)
	}
}

func TestFileCredentialsRejectsIncompleteConfigurations(t *testing.T) {
	cases := map[string]string{
		"unknown context": `
apiVersion: v1
kind: Config
current-context: missing
contexts: []
clusters: []
users: []
`,
		"no credential": `
apiVersion: v1
kind: Config
current-context: c
clusters:
  - name: k
    cluster: {server: "https://a.example:6443"}
contexts:
  - name: c
    context: {cluster: k, user: u}
users:
  - name: u
    user: {}
`,
		"no server": `
apiVersion: v1
kind: Config
current-context: c
clusters:
  - name: k
    cluster: {}
contexts:
  - name: c
    context: {cluster: k, user: u}
users:
  - name: u
    user: {token: t}
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadCredentials(writeKubeconfig(t, body), ""); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLoadCredentialsWithoutKubeconfigNeedsTheClusterEnvironment(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	_, err := loadCredentials("", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "KUBERNETES_SERVICE_HOST") {
		t.Fatalf("error should name what is missing, got %q", err)
	}
}

func TestRelativeCertificatePathsResolveAgainstTheKubeconfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("ca-pem"), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	path := filepath.Join(dir, "kubeconfig")
	body := `
apiVersion: v1
kind: Config
current-context: c
clusters:
  - name: k
    cluster:
      server: https://a.example:6443
      certificate-authority: ca.crt
contexts:
  - name: c
    context: {cluster: k, user: u}
users:
  - name: u
    user: {token: t}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	creds, err := loadCredentials(path, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(creds.caPEM) != "ca-pem" {
		t.Fatalf("ca = %q", creds.caPEM)
	}
}
