package kubernetes

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// serviceAccountDir is where the kubelet mounts an in-cluster credential.
const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// credentials is everything needed to reach one API server.
type credentials struct {
	server    string
	namespace string
	token     string
	username  string
	password  string
	caPEM     []byte
	cert      *tls.Certificate
	insecure  bool
}

// kubeconfigFile is the subset of the kubeconfig schema this adapter needs.
//
// Client-go is deliberately not a dependency: the control plane stays a single
// static binary built from yaml.v3 and a pure-Go SQLite driver, and the four
// Pod verbs the adapter uses do not justify pulling in the whole client.
type kubeconfigFile struct {
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string `yaml:"token"`
			TokenFile             string `yaml:"tokenFile"`
			ClientCertificate     string `yaml:"client-certificate"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKey             string `yaml:"client-key"`
			ClientKeyData         string `yaml:"client-key-data"`
		} `yaml:"user"`
	} `yaml:"users"`
	CurrentContext string `yaml:"current-context"`
}

// loadCredentials resolves the API server credential. An empty kubeconfig path
// means "running inside the cluster": the projected service account token is
// the only credential the control plane then needs.
func loadCredentials(kubeconfigPath, contextName string) (credentials, error) {
	if strings.TrimSpace(kubeconfigPath) == "" {
		return inClusterCredentials()
	}
	return fileCredentials(kubeconfigPath, contextName)
}

func inClusterCredentials() (credentials, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return credentials{}, errors.New("kubeconfig is empty and KUBERNETES_SERVICE_HOST/KUBERNETES_SERVICE_PORT are unset")
	}
	token, err := os.ReadFile(filepath.Join(serviceAccountDir, "token"))
	if err != nil {
		return credentials{}, fmt.Errorf("read in-cluster service account token: %w", err)
	}
	// The CA bundle is advisory: an API server behind a private CA that the
	// operator registered out of band still works with an explicit ca_path.
	ca, err := os.ReadFile(filepath.Join(serviceAccountDir, "ca.crt"))
	if err != nil {
		ca = nil
	}
	namespace := ""
	if raw, err := os.ReadFile(filepath.Join(serviceAccountDir, "namespace")); err == nil {
		namespace = strings.TrimSpace(string(raw))
	}
	return credentials{
		server:    "https://" + net.JoinHostPort(host, port),
		namespace: namespace,
		token:     strings.TrimSpace(string(token)),
		caPEM:     ca,
	}, nil
}

func fileCredentials(path, contextName string) (credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return credentials{}, fmt.Errorf("read kubeconfig %s: %w", path, err)
	}
	var file kubeconfigFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return credentials{}, fmt.Errorf("parse kubeconfig %s: %w", path, err)
	}
	if contextName == "" {
		contextName = file.CurrentContext
	}
	if contextName == "" {
		return credentials{}, errors.New("kubeconfig has no current-context and none was configured")
	}
	var clusterName, userName, namespace string
	found := false
	for _, entry := range file.Contexts {
		if entry.Name == contextName {
			clusterName, userName, namespace = entry.Context.Cluster, entry.Context.User, entry.Context.Namespace
			found = true
			break
		}
	}
	if !found {
		return credentials{}, fmt.Errorf("kubeconfig has no context %q", contextName)
	}

	dir := filepath.Dir(path)
	resolve := func(value string) string {
		if value == "" || filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(dir, value)
	}

	result := credentials{namespace: namespace}
	clusterFound := false
	for _, entry := range file.Clusters {
		if entry.Name != clusterName {
			continue
		}
		clusterFound = true
		result.server = strings.TrimRight(entry.Cluster.Server, "/")
		result.insecure = entry.Cluster.InsecureSkipTLSVerify
		switch {
		case entry.Cluster.CertificateAuthorityData != "":
			decoded, err := decodeBase64(entry.Cluster.CertificateAuthorityData, "certificate-authority-data")
			if err != nil {
				return credentials{}, err
			}
			result.caPEM = decoded
		case entry.Cluster.CertificateAuthority != "":
			decoded, err := os.ReadFile(resolve(entry.Cluster.CertificateAuthority))
			if err != nil {
				return credentials{}, fmt.Errorf("read certificate-authority: %w", err)
			}
			result.caPEM = decoded
		}
		break
	}
	if !clusterFound {
		return credentials{}, fmt.Errorf("kubeconfig has no cluster %q", clusterName)
	}
	if result.server == "" {
		return credentials{}, fmt.Errorf("kubeconfig cluster %q has no server", clusterName)
	}

	for _, entry := range file.Users {
		if entry.Name != userName {
			continue
		}
		result.token = entry.User.Token
		if result.token == "" && entry.User.TokenFile != "" {
			decoded, err := os.ReadFile(resolve(entry.User.TokenFile))
			if err != nil {
				return credentials{}, fmt.Errorf("read tokenFile: %w", err)
			}
			result.token = strings.TrimSpace(string(decoded))
		}
		certPEM, keyPEM, err := userCertificate(entry.User.ClientCertificateData, entry.User.ClientKeyData, entry.User.ClientCertificate, entry.User.ClientKey, resolve)
		if err != nil {
			return credentials{}, err
		}
		if len(certPEM) > 0 && len(keyPEM) > 0 {
			cert, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				return credentials{}, fmt.Errorf("kubeconfig client certificate: %w", err)
			}
			result.cert = &cert
		}
		break
	}
	if result.token == "" && result.cert == nil {
		return credentials{}, fmt.Errorf("kubeconfig user %q carries neither a token nor a client certificate", userName)
	}
	return result, nil
}

func userCertificate(certData, keyData, certFile, keyFile string, resolve func(string) string) ([]byte, []byte, error) {
	var certPEM, keyPEM []byte
	var err error
	switch {
	case certData != "":
		if certPEM, err = decodeBase64(certData, "client-certificate-data"); err != nil {
			return nil, nil, err
		}
	case certFile != "":
		if certPEM, err = os.ReadFile(resolve(certFile)); err != nil {
			return nil, nil, fmt.Errorf("read client-certificate: %w", err)
		}
	}
	switch {
	case keyData != "":
		if keyPEM, err = decodeBase64(keyData, "client-key-data"); err != nil {
			return nil, nil, err
		}
	case keyFile != "":
		if keyPEM, err = os.ReadFile(resolve(keyFile)); err != nil {
			return nil, nil, fmt.Errorf("read client-key: %w", err)
		}
	}
	return certPEM, keyPEM, nil
}

func decodeBase64(value, field string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s is not valid base64: %w", field, err)
	}
	return decoded, nil
}

// tlsConfig turns the credential into a transport configuration.
func (c credentials) tlsConfig() (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.insecure} //nolint:gosec // opt-in via kubeconfig
	if len(c.caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(c.caPEM) {
			return nil, errors.New("kubeconfig certificate authority is not valid PEM")
		}
		config.RootCAs = pool
	}
	if c.cert != nil {
		config.Certificates = []tls.Certificate{*c.cert}
	}
	return config, nil
}
