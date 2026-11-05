package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// ServerConfig is the server-side TLS configuration (PEM file paths).
type ServerConfig struct {
	// CertFile is the PEM server certificate chain (leaf first).
	CertFile string `yaml:"cert_file" envconfig:"CERT_FILE"`
	// KeyFile is the PEM private key for CertFile.
	KeyFile string `yaml:"key_file" envconfig:"KEY_FILE"`
	// ClientCAFile, when set, turns on mTLS: clients must present a certificate signed by this CA bundle (tls.RequireAndVerifyClientCert).
	ClientCAFile string `yaml:"client_ca_file" envconfig:"CLIENT_CA_FILE"`
}

// Enabled reports whether server TLS is configured.
func (c ServerConfig) Enabled() bool { return c.CertFile != "" }

// Validate checks for structurally invalid combinations.
func (c ServerConfig) Validate() error {
	if (c.CertFile == "") != (c.KeyFile == "") {
		return errors.New("tls server: cert_file and key_file must be set together")
	}
	if c.ClientCAFile != "" && c.CertFile == "" {
		return errors.New("tls server: client_ca_file requires cert_file and key_file")
	}
	return nil
}

// Build returns the server *tls.Config, or (nil, nil) when TLS is disabled.
//
// When ClientCAFile is set it enforces mTLS (RequireAndVerifyClientCert). Use this
// for agent-facing listeners (discovery). For browser-facing listeners (the UI) use
// [ServerConfig.BuildNoClientAuth] so enabling mTLS for agents does not lock out a
// browser, which has no client certificate.
func (c ServerConfig) Build() (*tls.Config, error) {
	return c.build(true)
}

// ClientCAs returns the certificates against which the listeners of Build
// verify client certificates, read from ClientCAFile as Build reads it, into
// the pool it builds: none while Build requires no client certificate, as
// server TLS is off or ClientCAFile is unset. A bundle that cannot be read, or
// holds no certificate, fails it as it fails Build.
func (c ServerConfig) ClientCAs() ([]*x509.Certificate, error) {
	if !c.Enabled() || c.ClientCAFile == "" {
		return nil, nil
	}
	return readCABundle(c.ClientCAFile)
}

// BuildWithClientCAs is Build with cas, the certificates that ClientCAs gave,
// in place of a read of ClientCAFile: a caller that read the bundle already,
// to digest it, builds the listeners' pool from the very certificates it
// digested, whatever the file holds by then. nil cas require no client
// certificate, as ClientCAs gives none without a ClientCAFile. With a
// ClientCAFile set they are an error instead, as they would leave open a
// listener that the configuration closes to clients without a certificate:
// the call fails closed. While server TLS is off the result is (nil, nil), as
// for Build. The key pair is read and fails as for Build.
func (c ServerConfig) BuildWithClientCAs(cas []*x509.Certificate) (*tls.Config, error) {
	cfg, err := c.build(false)
	if err != nil || cfg == nil {
		return cfg, err
	}
	if cas == nil {
		if c.ClientCAFile != "" {
			return nil, errors.New("tls server: client CA: client_ca_file is set, but no certificates were given")
		}
		return cfg, nil
	}
	cfg.ClientCAs = certPool(cas)
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return cfg, nil
}

// BuildNoClientAuth returns the server *tls.Config WITHOUT requiring a client
// certificate, even when ClientCAFile is set. This scopes mTLS to the agent-facing
// listeners while keeping the human-facing UI reachable from a browser.
func (c ServerConfig) BuildNoClientAuth() (*tls.Config, error) {
	return c.build(false)
}

func (c ServerConfig) build(requireClientCert bool) (*tls.Config, error) {
	if !c.Enabled() {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls server: load keypair: %w", err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if requireClientCert {
		cas, err := c.ClientCAs()
		if err != nil {
			return nil, fmt.Errorf("tls server: client CA: %w", err)
		}
		if cas != nil {
			cfg.ClientCAs = certPool(cas)
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}
	return cfg, nil
}

// ClientConfig is the client-side TLS configuration (PEM file paths) used by the CP when dialing agents.
type ClientConfig struct {
	CAFile   string `yaml:"ca_file"   envconfig:"CA_FILE"`
	CertFile string `yaml:"cert_file" envconfig:"CERT_FILE"`
	KeyFile  string `yaml:"key_file"  envconfig:"KEY_FILE"`
}

// Enabled reports whether the CP should dial agents over TLS.
func (c ClientConfig) Enabled() bool { return c.CAFile != "" || c.CertFile != "" }

// Validate checks for structurally invalid combinations.
func (c ClientConfig) Validate() error {
	if (c.CertFile == "") != (c.KeyFile == "") {
		return errors.New("tls client: cert_file and key_file must be set together")
	}
	return nil
}

// Build returns the client *tls.Config, or (nil, nil) when TLS is disabled.
func (c ClientConfig) Build() (*tls.Config, error) {
	if !c.Enabled() {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pool, err := loadCAPool(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("tls client: CA: %w", err)
		}
		cfg.RootCAs = pool
	}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tls client: load keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// loadCAPool reads a PEM CA bundle into a *x509.CertPool.
func loadCAPool(path string) (*x509.CertPool, error) {
	certs, err := readCABundle(path)
	if err != nil {
		return nil, err
	}
	return certPool(certs), nil
}

// readCABundle reads the certificates of the PEM bundle at path as
// x509.CertPool.AppendCertsFromPEM reads them: every CERTIFICATE block without
// headers that parses, in order; other blocks are skipped. A bundle that holds
// none is an error, as a file that cannot be read is.
func readCABundle(path string) ([]*x509.Certificate, error) {
	rest, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificates found in %s", path)
	}
	return certs, nil
}

// certPool is a pool of certs, which it holds as AppendCertsFromPEM would.
func certPool(certs []*x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return pool
}
