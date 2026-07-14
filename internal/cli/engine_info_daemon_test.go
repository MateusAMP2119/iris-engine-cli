package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
	"github.com/MateusAMP2119/iris-engine-cli/internal/daemon"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// startInfoDaemon stands up an in-process daemon over a unix socket serving the
// REAL api mux with the given info handler -- the integration-tier "in-process
// daemon over a socket" pattern -- so `iris engine info` reads the daemon-held
// runtime readout through the real GET /info route.
func startInfoDaemon(t *testing.T, sock string, handler api.InfoHandler) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: api.NewMux(api.WithInfo(handler)), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// infoJSON is the `iris engine info --json` data document as the readout tests
// decode it: the merged local-configuration and daemon-held runtime fields.
type infoJSON struct {
	Version         string `json:"version"`
	Go              string `json:"go"`
	Socket          string `json:"socket"`
	TCP             string `json:"tcp"`
	Mode            string `json:"mode"`
	ObjectsPath     string `json:"objects_path"`
	EngineKeyPublic string `json:"engine_key_public"`
	Role            string `json:"role"`
	Leader          string `json:"leader"`
	DataTarget      string `json:"data_target"`
	MetaTarget      string `json:"meta_target"`
	LanePasses      []struct {
		Lane   string `json:"lane"`
		Passes int64  `json:"passes"`
	} `json:"lane_passes"`
	Uptime string `json:"uptime"`
}

// TestEngineInfoReadout proves the `iris engine info` readout against a real mux
// over a unix socket: the full field set, and the leadership role with the
// leader named when known.
func TestEngineInfoReadout(t *testing.T) {
	t.Setenv("IRIS_HOST", "")
	t.Setenv("IRIS_SOCKET", "")
	t.Setenv("IRIS_TOKEN", "")

	key, err := daemon.MintEngineKey()
	if err != nil {
		t.Fatalf("MintEngineKey: %v", err)
	}
	newInstalledApp := func(out, errOut *bytes.Buffer) *app {
		a := newApp(out, errOut)
		a.newKeyReader = func(config.Settings) daemon.EngineKeyReader { return fakeKeyReader{key: key} }
		return a
	}

	// The daemon-held runtime readout: a leader with a TCP listener and two lanes
	// of counted passes.
	newLeaderPlane := func(sock string) api.InfoHandler {
		role := api.NewRoleState()
		role.SetLeader()
		pc := dispatch.NewPassCounter()
		pc.Hook()(dispatch.PassReport{Lane: "ingest"})
		pc.Hook()(dispatch.PassReport{Lane: "ingest"})
		pc.Hook()(dispatch.PassReport{Lane: "publish"})
		return daemon.NewInfoPlane(role, pc, daemon.InfoConfig{Socket: sock, TCP: "127.0.0.1:7433"})
	}

	t.Run("info-readout-fields", func(t *testing.T) {
		sock := shortSocket(t)
		startInfoDaemon(t, sock, newLeaderPlane(sock))

		var out, errb bytes.Buffer
		code := newInstalledApp(&out, &errb).run([]string{"--socket", sock, "engine", "info", "--json"})
		if code != exitOK {
			t.Fatalf("engine info exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
		}
		var doc struct {
			Data infoJSON `json:"data"`
		}
		decodeSingleJSON(t, out.Bytes(), &doc)
		d := doc.Data

		if d.Version == "" {
			t.Error("info reports no engine version")
		}
		if d.Go != runtime.Version() {
			t.Errorf("info go version = %q, want %q", d.Go, runtime.Version())
		}
		if d.Socket != sock {
			t.Errorf("info socket listener = %q, want %q", d.Socket, sock)
		}
		if d.TCP != "127.0.0.1:7433" {
			t.Errorf("info tcp listener = %q, want %q", d.TCP, "127.0.0.1:7433")
		}
		if d.DataTarget != pg.DataDatabase {
			t.Errorf("info data target = %q, want %q", d.DataTarget, pg.DataDatabase)
		}
		if d.MetaTarget != store.MetaDatabase {
			t.Errorf("info meta target = %q, want %q", d.MetaTarget, store.MetaDatabase)
		}
		if d.Role != "leader" {
			t.Errorf("info role = %q, want leader", d.Role)
		}
		if d.ObjectsPath == "" {
			t.Error("info reports no objects path")
		}
		if d.EngineKeyPublic != key.PublicBase64() {
			t.Errorf("info engine key public half = %q, want %q", d.EngineKeyPublic, key.PublicBase64())
		}
		wantPasses := map[string]int64{"ingest": 2, "publish": 1}
		if len(d.LanePasses) != len(wantPasses) {
			t.Fatalf("info lane passes = %+v, want one entry per lane %v", d.LanePasses, wantPasses)
		}
		for _, lp := range d.LanePasses {
			if wantPasses[lp.Lane] != lp.Passes {
				t.Errorf("info lane %q passes = %d, want %d", lp.Lane, lp.Passes, wantPasses[lp.Lane])
			}
		}
		if d.Uptime == "" {
			t.Error("info reports no uptime")
		}
	})

	t.Run("engine-info-reports-role", func(t *testing.T) {
		t.Run("leader reports leader", func(t *testing.T) {
			sock := shortSocket(t)
			startInfoDaemon(t, sock, newLeaderPlane(sock))

			var out, errb bytes.Buffer
			code := newInstalledApp(&out, &errb).run([]string{"--socket", sock, "engine", "info"})
			if code != exitOK {
				t.Fatalf("engine info exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			if !strings.Contains(out.String(), "leader") {
				t.Errorf("leader's engine info does not name its role:\n%s", out.String())
			}
		})

		t.Run("standby reports standby naming the leader", func(t *testing.T) {
			sock := shortSocket(t)
			role := api.NewRoleState()
			role.SetStandby("10.9.8.7:7433")
			startInfoDaemon(t, sock, daemon.NewInfoPlane(role, nil, daemon.InfoConfig{Socket: sock}))

			var out, errb bytes.Buffer
			code := newInstalledApp(&out, &errb).run([]string{"--socket", sock, "engine", "info", "--json"})
			if code != exitOK {
				t.Fatalf("engine info exit = %d, want %d\nstderr: %s", code, exitOK, errb.String())
			}
			var doc struct {
				Data infoJSON `json:"data"`
			}
			decodeSingleJSON(t, out.Bytes(), &doc)
			if doc.Data.Role != "standby" {
				t.Errorf("standby info role = %q, want standby", doc.Data.Role)
			}
			if doc.Data.Leader != "10.9.8.7:7433" {
				t.Errorf("standby info leader = %q, want the known leader 10.9.8.7:7433", doc.Data.Leader)
			}

			// The human readout names role and leader too.
			var hout, herr bytes.Buffer
			code = newInstalledApp(&hout, &herr).run([]string{"--socket", sock, "engine", "info"})
			if code != exitOK {
				t.Fatalf("engine info exit = %d, want %d\nstderr: %s", code, exitOK, herr.String())
			}
			if !strings.Contains(hout.String(), "standby") || !strings.Contains(hout.String(), "10.9.8.7:7433") {
				t.Errorf("standby's engine info does not name its role and the leader:\n%s", hout.String())
			}
		})
	})
}
