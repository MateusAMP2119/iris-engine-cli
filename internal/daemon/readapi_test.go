package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pat"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// This file proves the transport half of the read API over the daemon's real
// listeners: HTTP/1.1 resource-shaped JSON GETs on the one server both listeners
// share, the auth split (ambient socket vs per-request bearer on TCP), the exact
// HTTP status matrix, and read service from a standby.

// scopedVerifier resolves each known token to its authority, so the TCP
// listener's per-route scope checks can be proven without the real PAT store.
type scopedVerifier struct{ tokens map[string]api.Authority }

// VerifyToken resolves the token to its configured authority.
func (v scopedVerifier) VerifyToken(_ context.Context, tok string) (api.Authority, error) {
	a, ok := v.tokens[tok]
	if !ok {
		return api.Authority{}, errNoMatch
	}
	return a, nil
}

// rosterPaths is the mounted engine-state roster, sample params filled in
// (mirrors the api package's roster test list).
var rosterPaths = []string{
	"/pipelines",
	"/pipelines/load_orders",
	"/pipelines/load_orders/gate",
	"/runs",
	"/runs/42",
	"/runs/42/trace",
	"/dead_letters",
	"/dead_letters/42",
	"/dead_letters/42/impact",
	"/lanes",
	"/dependencies",
	"/workload",
	"/leader",
	"/ps",
	"/healthz",
	"/provenance/analytics/orders/123",
}

// wireEnvelope is the decoded read-API document for these listener tests.
type wireEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// fetch issues method+url through client with an optional bearer and returns the
// response and its decoded envelope. A transport-level failure fails the test.
func fetch(t *testing.T, client *http.Client, method, url, authz string) (*http.Response, wireEnvelope) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, url, err)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, url, err)
	}
	var env wireEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%s %s: body is not a JSON envelope: %v (%q)", method, url, err, body)
	}
	return resp, env
}

// fullAuthority is a PAT authority carrying every scope.
func fullAuthority(id string) api.Authority {
	return api.Authority{PATID: id, Scopes: []pat.Scope{pat.ScopeControl, pat.ScopeRead, pat.ScopeData}}
}

// startReadServer builds and starts a daemon Server with both listeners, a
// leader-role mux (opts appended), and the given verifier tokens.
func startReadServer(t *testing.T, role *api.RoleState, tokens map[string]api.Authority, opts ...api.MuxOption) (*Server, string, *http.Client) {
	t.Helper()
	sock := shortSocket(t)
	mux := api.NewMux(append([]api.MuxOption{api.WithRole(role)}, opts...)...)
	srv := NewServer(
		config.Settings{Socket: sock, TCP: "127.0.0.1:0"},
		mux,
		WithVerifier(scopedVerifier{tokens: tokens}),
	)
	startServer(t, srv)
	return srv, sock, unixClient(sock)
}

// TestHTTPGetJSONBothListeners proves the daemon serves the read API as HTTP/1.1
// resource-shaped JSON GETs on the same server as the control plane, reachable over
// both the unix socket and the optional TCP listener.
func TestHTTPGetJSONBothListeners(t *testing.T) {
	t.Run("http-get-json-both-listeners", func(t *testing.T) {
		role := api.NewRoleState()
		role.SetLeader()
		srv, _, socketClient := startReadServer(t, role,
			map[string]api.Authority{"tok": fullAuthority("p1")})
		tcpClient := &http.Client{}
		base := "http://" + srv.TCPAddr()

		for _, l := range []struct {
			name   string
			client *http.Client
			url    string
			authz  string
		}{
			{"unix socket", socketClient, "http://iris/leader", ""},
			{"tcp", tcpClient, base + "/leader", "Bearer tok"},
		} {
			t.Run(l.name, func(t *testing.T) {
				resp, env := fetch(t, l.client, http.MethodGet, l.url, l.authz)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("GET /leader over %s = %d, want 200", l.name, resp.StatusCode)
				}
				if resp.Proto != "HTTP/1.1" {
					t.Errorf("protocol over %s = %q, want HTTP/1.1", l.name, resp.Proto)
				}
				if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type over %s = %q, want application/json", l.name, ct)
				}
				if env.Data == nil {
					t.Errorf("GET /leader over %s carries no data envelope", l.name)
				}
			})
		}

		// The control plane rides the same server: the mutation route answers on
		// the same listeners the read API answers on (here: reached and routed,
		// which a 404 would disprove).
		resp, _ := fetch(t, socketClient, http.MethodPost, "http://iris/apply", "")
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("POST /apply over the socket = 404; the control plane must share the read API's server")
		}
	})
}

// TestTransportAuthSocketVsTCP proves the transport auth split: socket requests are
// ambiently authorized -- no token, full authority -- while every TCP request must
// present Authorization: Bearer <token>, with 401 unauthorized for a missing or bad
// token.
func TestTransportAuthSocketVsTCP(t *testing.T) {
	t.Run("transport-auth-socket-vs-tcp", func(t *testing.T) {
		role := api.NewRoleState()
		role.SetLeader()
		srv, _, socketClient := startReadServer(t, role,
			map[string]api.Authority{"tok": fullAuthority("p1")})
		tcpClient := &http.Client{}
		base := "http://" + srv.TCPAddr()

		// Socket: ambient. No Authorization header, yet fully served -- including
		// scope-checked routes on both surfaces.
		for _, path := range []string{"/healthz", "/leader"} {
			resp, _ := fetch(t, socketClient, http.MethodGet, "http://iris"+path, "")
			if resp.StatusCode != http.StatusOK {
				t.Errorf("ambient socket GET %s = %d, want 200", path, resp.StatusCode)
			}
		}

		// TCP: a missing token is 401 unauthorized.
		resp, env := fetch(t, tcpClient, http.MethodGet, base+"/healthz", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("TCP without token = %d, want 401", resp.StatusCode)
		}
		if env.Error == nil || env.Error.Code != "unauthorized" {
			t.Errorf("TCP 401 envelope = %+v, want code unauthorized", env.Error)
		}

		// TCP: a bad token is 401 unauthorized.
		resp, env = fetch(t, tcpClient, http.MethodGet, base+"/healthz", "Bearer wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("TCP with bad token = %d, want 401", resp.StatusCode)
		}
		if env.Error == nil || env.Error.Code != "unauthorized" {
			t.Errorf("TCP bad-token envelope = %+v, want code unauthorized", env.Error)
		}

		// TCP: a malformed Authorization header (not Bearer) is 401 too.
		resp, _ = fetch(t, tcpClient, http.MethodGet, base+"/healthz", "Basic dXNlcjpwYXNz")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("TCP with non-bearer auth = %d, want 401", resp.StatusCode)
		}

		// TCP: the valid token is served.
		resp, _ = fetch(t, tcpClient, http.MethodGet, base+"/healthz", "Bearer tok")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("TCP with valid token = %d, want 200", resp.StatusCode)
		}
	})
}

// matrixPs is a ps handler that serves an empty readout, so GET /ps can
// prove a 200; fail proves the 500 half.
type matrixPs struct{ fail bool }

// Ps serves an empty payload, or an engine fault when fail is set.
func (s matrixPs) Ps(context.Context, bool, bool) (api.PsPayload, error) {
	if s.fail {
		return api.PsPayload{}, errNoMatch
	}
	return api.PsPayload{}, nil
}

// TestHTTPStatusMatrix proves the exact status matrix: 200 success, 400
// malformed/unknown/repeated param, 401 missing or bad token on TCP, 403 missing
// scope, 404 unknown endpoint, 405 non-GET, 500 engine fault -- each carrying its
// closed-set error code.
func TestHTTPStatusMatrix(t *testing.T) {
	t.Run("http-status-matrix", func(t *testing.T) {
		role := api.NewRoleState()
		role.SetLeader()
		srv, _, socketClient := startReadServer(t, role,
			map[string]api.Authority{
				"data-tok": {PATID: "d1", Scopes: []pat.Scope{pat.ScopeData}},
				"full-tok": fullAuthority("p1"),
			},
			api.WithPs(matrixPs{}),
		)
		tcpClient := &http.Client{}
		base := "http://" + srv.TCPAddr()

		t.Run("200 success", func(t *testing.T) {
			for _, path := range []string{"/healthz", "/leader", "/ps"} {
				resp, env := fetch(t, socketClient, http.MethodGet, "http://iris"+path, "")
				if resp.StatusCode != http.StatusOK || env.Data == nil {
					t.Errorf("GET %s = %d (data %v), want 200 with a data envelope", path, resp.StatusCode, env.Data != nil)
				}
			}
		})

		t.Run("400 unknown param, named", func(t *testing.T) {
			resp, env := fetch(t, socketClient, http.MethodGet, "http://iris/healthz?bogus=1", "")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("GET /healthz?bogus=1 = %d, want 400", resp.StatusCode)
			}
			if env.Error == nil || env.Error.Code != "bad_param" {
				t.Fatalf("400 envelope = %+v, want code bad_param", env.Error)
			}
			if !strings.Contains(env.Error.Message, "bogus") {
				t.Errorf("400 message %q does not name the offending param", env.Error.Message)
			}
		})

		t.Run("401 missing or bad token on TCP", func(t *testing.T) {
			for _, authz := range []string{"", "Bearer nope"} {
				resp, env := fetch(t, tcpClient, http.MethodGet, base+"/healthz", authz)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("TCP authz %q = %d, want 401", authz, resp.StatusCode)
					continue
				}
				if env.Error == nil || env.Error.Code != "unauthorized" {
					t.Errorf("401 envelope = %+v, want code unauthorized", env.Error)
				}
			}
		})

		t.Run("403 missing scope", func(t *testing.T) {
			resp, env := fetch(t, tcpClient, http.MethodGet, base+"/runs", "Bearer data-tok")
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("data-only GET /runs = %d, want 403", resp.StatusCode)
			}
			if env.Error == nil || env.Error.Code != "forbidden" {
				t.Errorf("403 envelope = %+v, want code forbidden", env.Error)
			}
		})

		t.Run("404 unknown endpoint", func(t *testing.T) {
			resp, env := fetch(t, socketClient, http.MethodGet, "http://iris/metrics", "")
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("GET /metrics = %d, want 404", resp.StatusCode)
			}
			if env.Error == nil || env.Error.Code != "not_found" {
				t.Errorf("404 envelope = %+v, want code not_found", env.Error)
			}
		})

		t.Run("405 non-GET", func(t *testing.T) {
			resp, env := fetch(t, socketClient, http.MethodPost, "http://iris/healthz", "")
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("POST /healthz = %d, want 405", resp.StatusCode)
			}
			if env.Error == nil || env.Error.Code != "method_not_allowed" {
				t.Errorf("405 envelope = %+v, want code method_not_allowed", env.Error)
			}
		})

		t.Run("500 engine fault", func(t *testing.T) {
			_, _, faultySocketClient := startReadServer(t, role, nil,
				api.WithPs(matrixPs{fail: true}))
			resp, env := fetch(t, faultySocketClient, http.MethodGet, "http://iris/ps", "")
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("GET /ps with a faulting readout = %d, want 500", resp.StatusCode)
			}
			if env.Error == nil || env.Error.Code != "internal" {
				t.Errorf("500 envelope = %+v, want code internal", env.Error)
			}
		})
	})
}

// TestStandbyServesReads proves a standby daemon serves every read route (reads
// work anywhere): the whole engine-state roster answers on a standby with exactly
// the status a leader gives, never a not_leader rejection.
func TestStandbyServesReads(t *testing.T) {
	t.Run("standby-serves-reads", func(t *testing.T) {
		leaderRole := api.NewRoleState()
		leaderRole.SetLeader()
		standbyRole := api.NewRoleState()
		standbyRole.SetStandby("10.0.0.7:9000")

		_, _, leaderClient := startReadServer(t, leaderRole, nil, api.WithPs(matrixPs{}))
		_, _, standbyClient := startReadServer(t, standbyRole, nil, api.WithPs(matrixPs{}))

		for _, path := range rosterPaths {
			leaderResp, _ := fetch(t, leaderClient, http.MethodGet, "http://iris"+path, "")
			standbyResp, env := fetch(t, standbyClient, http.MethodGet, "http://iris"+path, "")

			if standbyResp.StatusCode == api.StatusNotLeader || (env.Error != nil && env.Error.Code == api.CodeNotLeader) {
				t.Errorf("GET %s on a standby was rejected as not_leader; reads work on any role", path)
				continue
			}
			if standbyResp.StatusCode != leaderResp.StatusCode {
				t.Errorf("GET %s: standby = %d, leader = %d; reads must serve identically regardless of role",
					path, standbyResp.StatusCode, leaderResp.StatusCode)
			}
		}

		// The wired read routes are genuinely served on the standby.
		for _, path := range []string{"/healthz", "/leader", "/ps"} {
			resp, _ := fetch(t, standbyClient, http.MethodGet, "http://iris"+path, "")
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s on a standby = %d, want 200", path, resp.StatusCode)
			}
		}
	})
}

// TestStatsReadPATAccess proves a monitor with only the read scope can fetch GET
// /ps over TCP.
func TestStatsReadPATAccess(t *testing.T) {
	t.Run("stats-read-pat-access", func(t *testing.T) {
		role := api.NewRoleState()
		role.SetLeader()
		readTok := "read-pat"
		_, _, _ = startReadServer(t, role,
			map[string]api.Authority{
				readTok: {PATID: "r1", Scopes: []pat.Scope{pat.ScopeRead}},
			},
			api.WithPs(matrixPs{}),
		)
		// We use the unix socket client for this claim (ambient full), but the
		// important is the scope matrix allows read for /ps. A dedicated TCP
		// read-only test is covered by the matrix in TestHTTPStatusMatrix using
		// data vs full; here we assert explicitly a read PAT path would be allowed
		// by requiredScope returning read.
		// To drive TCP read, recreate with TCP client.
		srv2, _, _ := startReadServer(t, role,
			map[string]api.Authority{
				readTok: {PATID: "r2", Scopes: []pat.Scope{pat.ScopeRead}},
			},
			api.WithPs(matrixPs{}),
		)
		tcp := &http.Client{}
		base := "http://" + srv2.TCPAddr()
		resp, env := fetch(t, tcp, http.MethodGet, base+"/ps", "Bearer "+readTok)
		if resp.StatusCode != http.StatusOK || env.Error != nil {
			t.Fatalf("read-scoped PAT GET /ps = %d err=%+v, want 200", resp.StatusCode, env.Error)
		}
	})
}

// nopMetaWrite is a store.MetaWriteConn for candidate wiring in the listener
// tests: a contended standby never wins the lock, so it issues no meta write and
// this connection is never exercised.
type nopMetaWrite struct{}

// Exec satisfies store.MetaWriteConn; a contended standby never calls it.
func (nopMetaWrite) Exec(context.Context, string, ...any) error { return nil }

// pollRole waits until cond holds or a short deadline passes, so a role
// transition is observed by condition, never by a fixed sleep (the S16
// no-fixed-sleeps doctrine): the brief interval only keeps the loop from
// spinning.
func pollRole(cond func() bool) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestCandidateBindsListeners proves every candidate binds its own listeners and
// serves reads regardless of the role it holds (every candidate binds its own
// listeners; reads work anywhere). It drives a real Candidate contending for a held
// lock -- so it stays a standby -- over the daemon's real unix listener, and shows
// the read roster answers on the standby.
func TestCandidateBindsListeners(t *testing.T) {
	t.Run("reads-work-on-any-candidate", func(t *testing.T) {
		// Contend with a held lock so this candidate stays a standby while we
		// prove its listeners are up and serving reads.
		set := storetest.NewLockSet()
		holder := set.New()
		if err := holder.Acquire(context.Background()); err != nil {
			t.Fatalf("holder acquire: %v", err)
		}

		role := api.NewRoleState()
		sock := shortSocket(t)
		mux := api.NewMux(api.WithRole(role))
		srv := NewServer(config.Settings{Socket: sock}, mux)
		startServer(t, srv)

		cand := NewCandidate(set.New(), role, nopMetaWrite{}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- cand.Serve(ctx) }()
		t.Cleanup(func() { cancel(); <-done })

		// The candidate reports standby on its first Serve iteration, before its
		// Acquire blocks on the held lock.
		if !pollRole(func() bool { return role.Role() == api.RoleStandby }) {
			t.Fatalf("candidate never reported standby; role=%s", role.Role())
		}

		// Listeners are bound and reads succeed on the standby-role candidate:
		// the same status a leader would give, never a not_leader rejection.
		client := unixClient(sock)
		for _, path := range []string{"/healthz", "/leader"} {
			resp, env := fetch(t, client, http.MethodGet, "http://iris"+path, "")
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s on a contending candidate = %d, want 200", path, resp.StatusCode)
			}
			if resp.StatusCode == api.StatusNotLeader || (env.Error != nil && env.Error.Code == api.CodeNotLeader) {
				t.Errorf("GET %s on a standby was rejected as not_leader; reads work on any role", path)
			}
		}
	})
}

// TestStandbyDaemonListenerRejectsMutations proves a mutation sent to a standby
// daemon's real listener is rejected with the not_leader envelope carrying the
// leader guidance. It drives a real Candidate contending for a held lock (so it
// stays a standby) and POSTs a mutation over the daemon's unix socket, asserting
// the 421 not_leader rejection and that the envelope carries the leader the role
// holds.
func TestStandbyDaemonListenerRejectsMutations(t *testing.T) {
	t.Run("standby-rejects-mutations-with-leader-guidance", func(t *testing.T) {
		set := storetest.NewLockSet()
		holder := set.New()
		if err := holder.Acquire(context.Background()); err != nil {
			t.Fatalf("holder acquire: %v", err)
		}

		role := api.NewRoleState()
		sock := shortSocket(t)
		mux := api.NewMux(api.WithRole(role))
		srv := NewServer(config.Settings{Socket: sock}, mux)
		startServer(t, srv)

		cand := NewCandidate(set.New(), role, nopMetaWrite{}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- cand.Serve(ctx) }()
		t.Cleanup(func() { cancel(); <-done })

		if !pollRole(func() bool { return role.Role() == api.RoleStandby }) {
			t.Fatalf("candidate did not reach standby; role=%s", role.Role())
		}

		// Record a known leader address on the standby role: the candidate is
		// blocked in Acquire and will not overwrite it, so the rejection envelope
		// must surface exactly this guidance.
		const leaderHint = "leader.example:9876"
		role.SetStandby(leaderHint)

		client := unixClient(sock)
		req, err := http.NewRequest(http.MethodPost, "http://iris/apply", strings.NewReader(`{"path":"some/iris-declare.yaml"}`))
		if err != nil {
			t.Fatalf("build POST: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST to standby listener: %v", err)
		}
		defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

		if resp.StatusCode != api.StatusNotLeader {
			t.Fatalf("standby listener mutation status = %d, want %d (not_leader)", resp.StatusCode, api.StatusNotLeader)
		}
		var env struct {
			Error struct {
				Code   string `json:"code"`
				Leader string `json:"leader"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode not_leader envelope: %v", err)
		}
		if env.Error.Code != api.CodeNotLeader {
			t.Errorf("error code = %q, want %q", env.Error.Code, api.CodeNotLeader)
		}
		if env.Error.Leader != leaderHint {
			t.Errorf("not_leader envelope leader = %q, want %q (guidance to the current leader)", env.Error.Leader, leaderHint)
		}
	})
}
