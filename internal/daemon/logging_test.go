package daemon_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
	"github.com/MateusAMP2119/iris-lakehouse/internal/daemon"
)

// TestStructuredJSONLogs proves the daemon emits structured JSON logs via slog when
// it runs as a daemon (detached / daemonized) and switches to human-readable
// console output when it runs attached in the foreground (structured JSON logs
// (slog); human console in foreground). LoggerFor is the pure constructor the
// lifecycle wiring calls; its output shape is asserted directly, and
// OpenDaemonLogger proves the daemon-mode logger lands JSON in the size-rotated
// daemon.log.
func TestStructuredJSONLogs(t *testing.T) {
	t.Run("structured-json-logs", func(t *testing.T) {
		t.Run("daemon mode emits structured JSON", func(t *testing.T) {
			var buf bytes.Buffer
			logger := daemon.LoggerFor(daemon.LogModeDaemon, &buf)
			logger.Info("iris daemon listening", "socket", "/tmp/iris.sock", "mode", "managed")

			line := strings.TrimSpace(buf.String())
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("daemon-mode log line is not JSON: %v\nline: %s", err, line)
			}
			if rec["msg"] != "iris daemon listening" {
				t.Errorf("msg = %v, want %q", rec["msg"], "iris daemon listening")
			}
			if rec["socket"] != "/tmp/iris.sock" {
				t.Errorf("socket = %v, want /tmp/iris.sock", rec["socket"])
			}
			if rec["level"] != "INFO" {
				t.Errorf("level = %v, want INFO", rec["level"])
			}
		})

		t.Run("foreground mode emits human-readable text, not JSON", func(t *testing.T) {
			var buf bytes.Buffer
			logger := daemon.LoggerFor(daemon.LogModeForeground, &buf)
			logger.Info("iris daemon listening", "socket", "/tmp/iris.sock")

			out := buf.String()
			var rec map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err == nil {
				t.Errorf("foreground log line parsed as JSON but must be human text: %s", out)
			}
			// slog's text handler renders key=value pairs on one line.
			if !strings.Contains(out, "msg=") || !strings.Contains(out, "iris daemon listening") {
				t.Errorf("foreground log is not human-readable key=value text: %s", out)
			}
			if !strings.Contains(out, "socket=/tmp/iris.sock") {
				t.Errorf("foreground log missing socket attribute: %s", out)
			}
		})

		t.Run("daemon-mode logger writes JSON to the size-rotated daemon.log", func(t *testing.T) {
			ws := t.TempDir()
			s := config.Resolve(config.Defaults(ws), config.Layer{}, config.Layer{}, config.Layer{})

			logger, closer, err := daemon.OpenDaemonLogger(s)
			if err != nil {
				t.Fatalf("OpenDaemonLogger: %v", err)
			}
			logger.Info("started", "n", 1)
			if err := closer.Close(); err != nil {
				t.Fatalf("close daemon logger: %v", err)
			}

			raw, err := os.ReadFile(daemon.LogPath(s))
			if err != nil {
				t.Fatalf("read daemon.log: %v", err)
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &rec); err != nil {
				t.Fatalf("daemon.log does not carry a JSON record: %v\n%s", err, raw)
			}
			if rec["msg"] != "started" {
				t.Errorf("daemon.log msg = %v, want started", rec["msg"])
			}
		})
	})
}
