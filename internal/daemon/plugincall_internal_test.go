package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
	"github.com/MateusAMP2119/iris-lakehouse/internal/plugin"
)

// installTestPlugin lays a real script tool plugin (verbs send, fail) into a
// temp root and returns the root.
func installTestPlugin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	src := t.TempDir()
	body := `#!/bin/sh
args=$(cat)
echo "plugin saw $1 $args" >&2
if [ "$1" = fail ]; then exit 3; fi
printf '{"message_id":"m-%s"}' "$1"
`
	if err := os.WriteFile(filepath.Join(src, "bin"), []byte(body), 0o755); err != nil { //nolint:gosec // test plugin must be executable
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`name: mailer
version: "1.0"
kind: tool
verbs:
  send:
    timeout_seconds: 10
  fail: {}
binaries:
  %s:
    url: ./bin
    sha256: "%s"
`, plugin.Platform(), plugin.Digest([]byte(body)))
	mp := filepath.Join(src, plugin.ManifestFile)
	if err := os.WriteFile(mp, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Install(context.Background(), root, mp, nil); err != nil {
		t.Fatalf("install test plugin: %v", err)
	}
	return root
}

// callScript is a protocol-speaking resident that calls mail.send once, then
// mail.fail, and echoes each reply to stderr before ending the turn.
const callScript = `while read line; do
  case "$line" in
  *'"go"'*)
    turn=$(printf '%s' "$line" | sed 's/.*"turn"://;s/[^0-9].*//')
    ;;
  *'"run"'*)
    printf '{"event":"call","call":1,"verb":"mail.send","args":{"to":"a@b.c"}}\n'
    ;;
  *'"call":1'*)
    echo "reply1 $line" >&2
    printf '{"event":"call","call":2,"verb":"mail.fail"}\n'
    ;;
  *'"call":2'*)
    echo "reply2 $line" >&2
    printf '{"event":"done","turn":%s}\n' "$turn"
    ;;
  esac
done
`

func TestResolveTurnPluginsRefusals(t *testing.T) {
	root := installTestPlugin(t)
	runner := exec.NewOSRunner()

	t.Run("no plugins resolves to nil", func(t *testing.T) {
		rp, err := resolveTurnPlugins(root, nil, runner, nil)
		if rp != nil || err != nil {
			t.Fatalf("= %+v, %v", rp, err)
		}
	})
	t.Run("unsupported lifetime", func(t *testing.T) {
		_, err := resolveTurnPlugins(root, map[string]declare.PluginUse{
			"mail": {Ref: "mailer@1.0", Lifetime: declare.LifetimeResident},
		}, runner, nil)
		if err == nil || !strings.Contains(err.Error(), "not yet supported") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not installed", func(t *testing.T) {
		_, err := resolveTurnPlugins(root, map[string]declare.PluginUse{
			"mail": {Ref: "ghost@9.9"},
		}, runner, nil)
		if err == nil || !strings.Contains(err.Error(), "not installed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("resolves pins and calls", func(t *testing.T) {
		rp, err := resolveTurnPlugins(root, map[string]declare.PluginUse{
			"mail": {Ref: "mailer@1.0"},
		}, runner, nil)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(rp.pins) != 1 || rp.pins[0].Alias != "mail" || rp.pins[0].Digest == "" {
			t.Fatalf("pins = %+v", rp.pins)
		}
		if !rp.calls["mail"]["send"] || !rp.calls["mail"]["fail"] {
			t.Fatalf("calls = %+v", rp.calls)
		}
	})
}

func TestDriveTurnServicesPluginCalls(t *testing.T) {
	ctx := context.Background()
	runner := exec.NewOSRunner()
	root := installTestPlugin(t)

	stderr := &lockedBuffer{}
	rp, err := resolveTurnPlugins(root, map[string]declare.PluginUse{"mail": {Ref: "mailer@1.0"}}, runner, stderr)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	dir := t.TempDir()
	argv := writeScript(t, dir, callScript)
	ses, err := spawnResident(ctx, runner, "k", dir, argv, nil)
	if err != nil {
		t.Fatalf("spawnResident: %v", err)
	}
	defer ses.end()
	ses.out.Set(stderr)

	res := driveTurn(ctx, ses, ses.nextTurn(), nil, testTurnWrites(), rp, nil)
	if res.kind != turnDone {
		t.Fatalf("turn = %+v, want done", res)
	}
	if len(res.calls) != 2 {
		t.Fatalf("calls = %+v, want two records", res.calls)
	}
	ok, fail := res.calls[0], res.calls[1]
	if ok.Seq != 1 || ok.Alias != "mail" || ok.Verb != "send" || ok.Outcome != "ok" ||
		ok.ArgsDigest == "" || ok.ResponseDigest == "" {
		t.Fatalf("ok record = %+v", ok)
	}
	if fail.Seq != 2 || fail.Verb != "fail" || fail.Outcome != "err" || !strings.Contains(fail.Error, "exited 3") {
		t.Fatalf("err record = %+v", fail)
	}

	// The pipeline saw both replies: the ok result and the err refusal.
	awaitOutput(t, stderr, `reply1 {"event":"res","call":1,"ok":true,"result":{"message_id":"m-send"}}`)
	awaitOutput(t, stderr, `"call":2,"ok":false`)
	// The plugin's own stderr rode the turn log.
	awaitOutput(t, stderr, `plugin saw send {"to":"a@b.c"}`)
}
