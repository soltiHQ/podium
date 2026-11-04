package tlsconf

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSelfSigned(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	writePEM(t, certPath, "CERTIFICATE", der)
	writePEM(t, keyPath, "PRIVATE KEY", keyDER)
	return certPath, keyPath
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
}

func TestServerConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ServerConfig
		wantErr bool
	}{
		{"empty is ok (plaintext)", ServerConfig{}, false},
		{"cert+key ok", ServerConfig{CertFile: "c", KeyFile: "k"}, false},
		{"cert without key", ServerConfig{CertFile: "c"}, true},
		{"key without cert", ServerConfig{KeyFile: "k"}, true},
		{"client CA without cert", ServerConfig{ClientCAFile: "ca"}, true},
		{"client CA with cert+key ok", ServerConfig{CertFile: "c", KeyFile: "k", ClientCAFile: "ca"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestServerConfigBuildDisabled(t *testing.T) {
	cfg, err := ServerConfig{}.Build()
	if err != nil || cfg != nil {
		t.Fatalf("disabled Build() = (%v, %v), want (nil, nil)", cfg, err)
	}
}

func TestServerConfigBuild(t *testing.T) {
	cert, key := writeSelfSigned(t)
	cfg, err := ServerConfig{CertFile: cert, KeyFile: key}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("Certificates = %d, want 1", len(cfg.Certificates))
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS1.2", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Fatalf("ClientAuth = %v, want NoClientCert (no mTLS)", cfg.ClientAuth)
	}
}

func TestServerConfigBuildMTLS(t *testing.T) {
	cert, key := writeSelfSigned(t)
	cfg, err := ServerConfig{CertFile: cert, KeyFile: key, ClientCAFile: cert}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Fatalf("ClientCAs is nil, want pool")
	}
}

// ClientCAs gives the certificates that the pool of Build holds, read from the
// bundle as Build reads it: every CERTIFICATE block without headers that
// parses, in order, other blocks skipped. It gives none while Build requires no
// client certificate, and fails as Build fails on a bundle it cannot read or
// that holds no certificate.
func TestServerConfigClientCAs(t *testing.T) {
	cert, key := writeSelfSigned(t)
	other, _ := writeSelfSigned(t)
	read := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	der := func(path string) []byte {
		block, _ := pem.Decode(read(path))
		return block.Bytes
	}
	bundle := filepath.Join(t.TempDir(), "bundle.pem")
	parts := [][]byte{
		read(cert),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a certificate")}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: der(other)}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("does not parse")}),
		read(other),
	}
	if err := os.WriteFile(bundle, bytes.Join(parts, nil), 0o600); err != nil {
		t.Fatal(err)
	}

	server := ServerConfig{CertFile: cert, KeyFile: key, ClientCAFile: bundle}
	got, err := server.ClientCAs()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !bytes.Equal(got[0].Raw, der(cert)) || !bytes.Equal(got[1].Raw, der(other)) {
		t.Fatalf("ClientCAs() gave %d certificates, want the two plain CERTIFICATE blocks in order", len(got))
	}
	if _, err := server.Build(); err != nil {
		t.Fatalf("Build() of the same bundle: %v", err)
	}

	for _, tc := range []struct {
		name string
		cfg  ServerConfig
	}{
		{"no client CA", ServerConfig{CertFile: cert, KeyFile: key}},
		{"no server TLS", ServerConfig{ClientCAFile: bundle}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := tc.cfg.ClientCAs(); got != nil || err != nil {
				t.Fatalf("ClientCAs() = %v, %v, want none", got, err)
			}
		})
	}

	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, file, want string
	}{
		{"a missing bundle", filepath.Join(t.TempDir(), "missing.pem"), "no such file"},
		{"a bundle without a certificate", empty, "no certificates found in " + empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ServerConfig{CertFile: cert, KeyFile: key, ClientCAFile: tc.file}
			if _, err := cfg.ClientCAs(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ClientCAs() = %v, want an error saying %q", err, tc.want)
			}
			if _, err := cfg.Build(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build() = %v, want an error saying %q", err, tc.want)
			}
		})
	}
}

func TestClientConfigBuild(t *testing.T) {
	cert, key := writeSelfSigned(t)

	t.Run("disabled", func(t *testing.T) {
		cfg, err := ClientConfig{}.Build()
		if err != nil || cfg != nil {
			t.Fatalf("disabled Build() = (%v, %v), want (nil, nil)", cfg, err)
		}
	})

	t.Run("ca only", func(t *testing.T) {
		cfg, err := ClientConfig{CAFile: cert}.Build()
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if cfg.RootCAs == nil {
			t.Fatalf("RootCAs is nil, want pool")
		}
		if len(cfg.Certificates) != 0 {
			t.Fatalf("Certificates = %d, want 0 (no client cert)", len(cfg.Certificates))
		}
	})

	t.Run("mtls client cert", func(t *testing.T) {
		cfg, err := ClientConfig{CAFile: cert, CertFile: cert, KeyFile: key}.Build()
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(cfg.Certificates) != 1 {
			t.Fatalf("Certificates = %d, want 1", len(cfg.Certificates))
		}
	})
}

func TestClientConfigValidate(t *testing.T) {
	if err := (ClientConfig{CertFile: "c"}).Validate(); err == nil {
		t.Fatalf("cert without key should error")
	}
	if err := (ClientConfig{CAFile: "ca"}).Validate(); err != nil {
		t.Fatalf("ca-only should be valid, got %v", err)
	}
}
