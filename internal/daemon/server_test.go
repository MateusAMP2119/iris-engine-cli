package daemon

import (
	"bytes"
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

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pat"
)

// fakeVerifier accepts exactly one bearer token, so the TCP listener's PAT gate
// can be proven to admit an authenticated request and reject the rest without the
// real PAT store behind it (the store-backed verifier in verifier.go).
type fakeVerifier struct {
	good   string
	scopes []pat.Scope
}

// VerifyToken accepts only the configured token, resolving it to the configured
// scopes (defaults to full if none specified).
func (f fakeVerifier) VerifyToken(_ context.Context, tok string) (api.Authority, error) {
	if tok == f.good {
		sc := f.scopes
		if len(sc) == 0 {
			sc = []pat.Scope{pat.ScopeControl, pat.ScopeRead, pat.ScopeData}
		}
		return api.Authority{PATID: "fake", Scopes: sc}, nil
	}
	return api.Authority{}, errNoMatch
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

// TestUnixSocketDefault proves the zero-config unix control socket: a Server with
// nothing configured but a socket path always listens on that unix socket, protects
// it with owner-only filesystem permissions, serves HTTP/JSON there needing no
// authentication (ambient, local), and brings up no TCP listener.
func TestUnixSocketDefault(t *testing.T) {
	t.Run("unix-socket-default", func(t *testing.T) {
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

// TestTCPOptInPATGated proves the opt-in, PAT-gated TCP listener: it stays off
// unless a TCP address is configured; once on, every request over it must
// authenticate with a PAT (missing bearer -> 401, valid bearer -> 200), while the
// sibling unix-socket request on the same server needs nothing.
func TestTCPOptInPATGated(t *testing.T) {
	t.Run("tcp-opt-in-pat-gated", func(t *testing.T) {
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
// pair (--tls-cert/--tls-key) is configured and plain TCP is served when they are
// absent.
func TestTLSWhenCertsGiven(t *testing.T) {
	t.Run("tls-when-certs-given", func(t *testing.T) {
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

// TestDestructiveOpsTCPReachable proves declare destroy (and by tiering the
// dev-loop destructive surfaces) are reachable over the TCP listener with a control
// PAT. They must pass the PAT transport gate and the mux leader/scope gates (not
// refused as unauthorized/forbidden/not_leader). 4xx/5xx from later handler or no
// route is acceptable; the contract is the remote surface tiering, in contrast to
// engine uninstall which is local-only.
func TestDestructiveOpsTCPReachable(t *testing.T) {
	t.Run("destructive-ops-tcp-reachable", func(t *testing.T) {
		sock := shortSocket(t)
		srv := NewServer(
			config.Settings{Socket: sock, TCP: "127.0.0.1:0"},
			api.NewMux(api.WithRole(leaderRole())),
			WithVerifier(fakeVerifier{good: "ctl", scopes: []pat.Scope{pat.ScopeControl}}),
		)
		startServer(t, srv)
		if !srv.TCPEnabled() {
			t.Fatal("TCP listener did not come up")
		}
		addr := "http://" + srv.TCPAddr()

		client := &http.Client{Timeout: 2 * time.Second}

		// declare destroy is wired as /destroy and must accept a confirmed
		// control-PAT request over TCP (not blocked at auth/leader/scope).
		destroyBody := []byte(`{"path":"ws/iris-declare.yaml","confirm":true}`)
		req, _ := http.NewRequest(http.MethodPost, addr+"/destroy", bytes.NewReader(destroyBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer ctl")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /destroy over TCP: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == api.StatusNotLeader {
			t.Errorf("declare destroy over TCP got blocked status %d; must be reachable with control PAT", resp.StatusCode)
		}
		// 422/404/500 etc ok: later layers or no real handler.

		// workload wipe and deadletter drain must likewise be reachable over TCP
		// with a control PAT (remote surface tiering); 4xx/5xx after the gate
		// are acceptable.
		for _, path := range []string{"/workload/wipe", "/deadletter/drain"} {
			body := []byte(`{"confirm":true}`)
			req, _ := http.NewRequest(http.MethodPost, addr+path, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer ctl")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("POST %s over TCP: %v", path, err)
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == api.StatusNotLeader {
				t.Errorf("%s over TCP got blocked status %d; destructive ops must be TCP-reachable with control PAT", path, resp.StatusCode)
			}
		}
	})
}

// leaderRole returns a RoleReporter that is the leader.
func leaderRole() api.RoleReporter {
	r := api.NewRoleState()
	r.SetLeader()
	return r
}
