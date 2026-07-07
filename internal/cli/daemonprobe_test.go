package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pat"
)

// acceptTokenVerifier accepts one bearer token, so a TCP probe over the PAT gate
// can reach /healthz and get 200 -- isolating the transport (http vs https) from
// the auth outcome.
type acceptTokenVerifier struct{ good string }

// VerifyToken accepts only the configured token, resolving it to a read-scope
// authority (the probe hits /healthz, an engine-state read).
func (v acceptTokenVerifier) VerifyToken(_ context.Context, tok string) (api.Authority, error) {
	if tok == v.good {
		return api.Authority{PATID: "probe", Scopes: []pat.Scope{pat.ScopeRead}}, nil
	}
	return api.Authority{}, errors.New("cli: bad token")
}

// startInProcess starts srv and registers its shutdown, failing on a bind error.
func startInProcess(t *testing.T, srv *daemon.Server) {
	t.Helper()
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start in-process daemon: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
}

// TestDaemonProbeSchemeAware proves the daemon-reachability probe is scheme-aware
// on the --host (TCP) path: an https:// host probes over TLS so a TLS-serving
// daemon (--tls-cert/--tls-key) is reached, a bare host:port stays plain HTTP,
// and an https:// probe against a plain daemon fails fast (exit 3) rather than
// silently reporting no-daemon while the daemon is up.
func TestDaemonProbeSchemeAware(t *testing.T) {
	// spec: S02/tls-when-certs-given
	t.Run("S02/tls-when-certs-given", func(t *testing.T) {
		certFile, keyFile, pool := selfSignedCert(t)

		t.Run("https host reaches a TLS daemon", func(t *testing.T) {
			srv := daemon.NewServer(
				config.Settings{Socket: shortSocket(t), TCP: "127.0.0.1:0", TLSCert: certFile, TLSKey: keyFile},
				api.NewMux(),
				daemon.WithVerifier(acceptTokenVerifier{good: "secret"}),
			)
			startInProcess(t, srv)

			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.daemonTLSConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
			code := a.run([]string{"--host", "https://" + srv.TCPAddr(), "--token", "secret", "pipeline", "list"})
			if code == exitNoDaemon {
				t.Fatalf("https host against a live TLS daemon reported no-daemon (exit 3)\nstderr: %s", errb.String())
			}
		})

		t.Run("bare host reaches a plain daemon", func(t *testing.T) {
			srv := daemon.NewServer(
				config.Settings{Socket: shortSocket(t), TCP: "127.0.0.1:0"},
				api.NewMux(),
				daemon.WithVerifier(acceptTokenVerifier{good: "secret"}),
			)
			startInProcess(t, srv)

			var out, errb bytes.Buffer
			code := newApp(&out, &errb).run([]string{"--host", srv.TCPAddr(), "--token", "secret", "pipeline", "list"})
			if code == exitNoDaemon {
				t.Fatalf("bare host against a live plain daemon reported no-daemon (exit 3)\nstderr: %s", errb.String())
			}
		})

		t.Run("https host against a plain daemon fails fast", func(t *testing.T) {
			srv := daemon.NewServer(
				config.Settings{Socket: shortSocket(t), TCP: "127.0.0.1:0"},
				api.NewMux(),
				daemon.WithVerifier(acceptTokenVerifier{good: "secret"}),
			)
			startInProcess(t, srv)

			var out, errb bytes.Buffer
			a := newApp(&out, &errb)
			a.daemonTLSConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
			code := a.run([]string{"--host", "https://" + srv.TCPAddr(), "--token", "secret", "pipeline", "list"})
			if code != exitNoDaemon {
				t.Fatalf("https against a plain daemon: exit = %d, want %d (TLS handshake must fail fast)\nstderr: %s",
					code, exitNoDaemon, errb.String())
			}
		})
	})
}

// selfSignedCert writes a self-signed ECDSA cert/key pair (valid for 127.0.0.1
// and localhost) to temp files and returns their paths plus a CertPool trusting
// the cert, so a test can drive the probe against a TLS daemon.
func selfSignedCert(t *testing.T) (certPath, keyPath string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "iris-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	pool = x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatalf("append cert to pool")
	}
	return certPath, keyPath, pool
}
