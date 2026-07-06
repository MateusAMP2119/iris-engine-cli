package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// fakeVerifier accepts exactly one bearer token, so the TCP listener's PAT gate
// can be proven to admit an authenticated request and reject the rest without the
// real (E09.1) PAT store.
type fakeVerifier struct{ good string }

// VerifyToken accepts only the configured token.
func (f fakeVerifier) VerifyToken(_ context.Context, tok string) error {
	if tok == f.good {
		return nil
	}
	return errNoMatch
}

var errNoMatch = errTest("unknown token")

type errTest string

func (e errTest) Error() string { return string(e) }

// shortSocket returns a unix-socket path under a fresh, short temp dir. t.TempDir
// embeds the (long) test name, which can overflow the platform's tight
// sockaddr_un limit (104 bytes on darwin); a short MkdirTemp keeps the path
// bindable.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "iris")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "iris.sock")
}

// startServer starts srv, registers its shutdown, and fails the test on a bind
// error.
func startServer(t *testing.T, srv *Server) {
	t.Helper()
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown() })
}

// getStatus issues a GET to url through client (with an optional Authorization
// header) and returns the HTTP status and the decoded healthz envelope status
// field, or (-1, error text) when the request itself fails.
func getStatus(t *testing.T, client *http.Client, url, authz string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := client.Do(req)
	if err != nil {
		return -1, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		Data struct {
			Status string `json:"status"`
			Role   string `json:"role"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &env)
	return resp.StatusCode, env.Data.Status
}

// unixClient returns an http.Client that dials the given unix socket for every
// request; request URLs use any host (the dialer ignores it).
func unixClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

// TestUnixSocketDefault proves the zero-config unix control socket
// (specification sections 2 and 10): a Server with nothing configured but a
// socket path always listens on that unix socket, protects it with owner-only
// filesystem permissions, serves HTTP/JSON there needing no authentication
// (ambient, local), and brings up no TCP listener.
func TestUnixSocketDefault(t *testing.T) {
	// spec: S02/unix-socket-default
	t.Run("S02/unix-socket-default", func(t *testing.T) {
		sock := shortSocket(t)
		srv := NewServer(config.Settings{Socket: sock}, api.NewMux())
		startServer(t, srv)

		// The socket file exists and is protected by filesystem permissions: owner
		// read/write only (0600), the local-only guard of a socket with no network
		// auth.
		info, err := os.Stat(sock)
		if err != nil {
			t.Fatalf("socket file not created at %s: %v", sock, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("socket permissions = %v, want 0600 (owner-only)", perm)
		}

		// HTTP/JSON is served over the socket with no token at all (ambient).
		code, status := getStatus(t, unixClient(sock), "http://iris/healthz", "")
		if code != http.StatusOK {
			t.Errorf("GET /healthz over socket: status = %d, want 200", code)
		}
		if status != "ok" {
			t.Errorf("healthz envelope status = %q, want ok", status)
		}

		// Zero config brings up no TCP listener.
		if srv.TCPEnabled() {
			t.Errorf("TCP listener is up with zero configuration; TCP is opt-in only")
		}
	})
}

// TestTCPOptInPATGated proves the opt-in, PAT-gated TCP listener (specification
// sections 2 and 7): it stays off unless a TCP address is configured; once on,
// every request over it must authenticate with a PAT (missing bearer -> 401,
// valid bearer -> 200), while the sibling unix-socket request on the same server
// needs nothing.
func TestTCPOptInPATGated(t *testing.T) {
	// spec: S02/tcp-opt-in-pat-gated
	t.Run("S02/tcp-opt-in-pat-gated", func(t *testing.T) {
		// Off by default: no TCP address configured, no TCP listener.
		off := NewServer(config.Settings{Socket: shortSocket(t)}, api.NewMux())
		startServer(t, off)
		if off.TCPEnabled() {
			t.Fatalf("TCP listener is up without --tcp/iris.toml opt-in")
		}

		// On when configured, gated by a PAT verifier.
		sock := shortSocket(t)
		on := NewServer(
			config.Settings{Socket: sock, TCP: "127.0.0.1:0"},
			api.NewMux(),
			WithVerifier(fakeVerifier{good: "secret"}),
		)
		startServer(t, on)
		if !on.TCPEnabled() {
			t.Fatalf("TCP listener is not up though --tcp was given")
		}
		base := "http://" + on.TCPAddr()
		tcpClient := &http.Client{}

		// Over TCP: a request without a bearer is 401.
		if code, _ := getStatus(t, tcpClient, base+"/healthz", ""); code != http.StatusUnauthorized {
			t.Errorf("TCP without bearer: status = %d, want 401", code)
		}
		// Over TCP: a wrong bearer is 401.
		if code, _ := getStatus(t, tcpClient, base+"/healthz", "Bearer wrong"); code != http.StatusUnauthorized {
			t.Errorf("TCP wrong bearer: status = %d, want 401", code)
		}
		// Over TCP: the valid bearer authenticates and is served.
		if code, status := getStatus(t, tcpClient, base+"/healthz", "Bearer secret"); code != http.StatusOK || status != "ok" {
			t.Errorf("TCP valid bearer: status = %d/%q, want 200/ok", code, status)
		}

		// The sibling unix socket on the same server is ambient: no token needed.
		if code, status := getStatus(t, unixClient(sock), "http://iris/healthz", ""); code != http.StatusOK || status != "ok" {
			t.Errorf("socket request needed auth: status = %d/%q, want 200/ok", code, status)
		}
	})
}

// TestTLSWhenCertsGiven proves TLS is served on the TCP listener when a cert/key
// pair is configured and plain TCP is served when they are absent (specification
// section 2: "--tls-cert/--tls-key ...; no certs: plain TCP").
func TestTLSWhenCertsGiven(t *testing.T) {
	// spec: S02/tls-when-certs-given
	t.Run("S02/tls-when-certs-given", func(t *testing.T) {
		certFile, keyFile, pool := selfSignedCertFiles(t)

		// With certs: the TCP listener speaks TLS. An HTTPS client that trusts the
		// cert completes the handshake and gets HTTP/JSON back.
		tlsSrv := NewServer(
			config.Settings{Socket: shortSocket(t), TCP: "127.0.0.1:0", TLSCert: certFile, TLSKey: keyFile},
			api.NewMux(),
			WithVerifier(fakeVerifier{good: "secret"}),
		)
		startServer(t, tlsSrv)
		tlsAddr := tlsSrv.TCPAddr()

		httpsClient := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
		if code, status := getStatus(t, httpsClient, "https://"+tlsAddr+"/healthz", "Bearer secret"); code != http.StatusOK || status != "ok" {
			t.Errorf("HTTPS over TLS listener: status = %d/%q, want 200/ok", code, status)
		}
		// A plain-HTTP request to the TLS listener cannot read HTTP/JSON: the
		// transport is TLS, so healthz is unreachable over plain HTTP.
		plainClient := &http.Client{Timeout: 3 * time.Second}
		if code, status := getStatus(t, plainClient, "http://"+tlsAddr+"/healthz", "Bearer secret"); code == http.StatusOK && status == "ok" {
			t.Errorf("plain HTTP served healthz over the TLS listener; the listener is not TLS")
		}

		// Without certs: the TCP listener is plain. A plain-HTTP client is served;
		// an HTTPS handshake against it cannot read HTTP/JSON.
		plainSrv := NewServer(
			config.Settings{Socket: shortSocket(t), TCP: "127.0.0.1:0"},
			api.NewMux(),
			WithVerifier(fakeVerifier{good: "secret"}),
		)
		startServer(t, plainSrv)
		plainAddr := plainSrv.TCPAddr()
		if code, status := getStatus(t, plainClient, "http://"+plainAddr+"/healthz", "Bearer secret"); code != http.StatusOK || status != "ok" {
			t.Errorf("plain HTTP over plain-TCP listener: status = %d/%q, want 200/ok", code, status)
		}
		if code, status := getStatus(t, httpsClient, "https://"+plainAddr+"/healthz", "Bearer secret"); code == http.StatusOK && status == "ok" {
			t.Errorf("HTTPS served healthz over the plain-TCP listener; it must be plain TCP")
		}
	})
}

// selfSignedCertFiles writes a self-signed ECDSA cert/key pair (valid for
// 127.0.0.1 and localhost) to temp files and returns their paths plus a CertPool
// trusting the cert, so a test HTTPS client can verify the daemon's TLS listener.
func selfSignedCertFiles(t *testing.T) (certPath, keyPath string, pool *x509.CertPool) {
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
