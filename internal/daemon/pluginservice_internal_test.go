package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/exec"
	"github.com/MateusAMP2119/iris-lakehouse/internal/plugin"
)

// serviceScriptBody is the line-protocol service the real-process tests
// supervise: count proves carried state (each call increments), boom answers
// err, hang never answers (the timeout leg).
const serviceScriptBody = `#!/bin/sh
count=0
while read line; do
  n=$(printf '%s' "$line" | sed 's/.*"call"://;s/[^0-9].*//')
  case "$line" in
  *'"verb":"count"'*)
    count=$((count+1))
    printf '{"call":%s,"ok":{"count":%s}}\n' "$n" "$count"
    ;;
  *'"verb":"boom"'*)
    printf '{"call":%s,"err":"kaput"}\n' "$n"
    ;;
  *'"verb":"hang"'*)
    sleep 5
    ;;
  esac
done
`

// installServicePlugin lays the line-protocol service plugin into a temp root.
func installServicePlugin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "bin"), []byte(serviceScriptBody), 0o755); err != nil { //nolint:gosec // test plugin must be executable
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`name: counter
version: "1.0"
kind: service
verbs:
  count: {}
  boom: {}
  hang:
    timeout_seconds: 1
binaries:
  %s:
    url: ./bin
    sha256: "%s"
`, plugin.Platform(), plugin.Digest([]byte(serviceScriptBody)))
	mp := filepath.Join(src, plugin.ManifestFile)
	if err := os.WriteFile(mp, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Install(context.Background(), root, mp, nil); err != nil {
		t.Fatalf("install service plugin: %v", err)
	}
	return root
}

// resolveCounter resolves the installed counter service for direct spawns.
func resolveCounter(t *testing.T, root string) *plugin.Resolved {
	t.Helper()
	res, err := plugin.Resolve(root, plugin.Ref{Name: "counter", Version: "1.0"})
	if err != nil {
		t.Fatalf("resolve counter: %v", err)
	}
	return res
}

func TestServiceSessionCallContract(t *testing.T) {
	ctx := context.Background()
	root := installServicePlugin(t)
	res := resolveCounter(t, root)

	t.Run("ok replies carry state across calls, err replies name the failure", func(t *testing.T) {
		s, err := spawnService(ctx, exec.NewOSRunner(), "counter@1.0#t1", res)
		if err != nil {
			t.Fatalf("spawnService: %v", err)
		}
		defer s.end()
		for want := 1; want <= 2; want++ {
			out, err := s.call(ctx, "count", nil, 5*time.Second)
			if err != nil {
				t.Fatalf("call %d: %v", want, err)
			}
			if string(out) != fmt.Sprintf(`{"count":%d}`, want) {
				t.Fatalf("call %d result = %s (one instance must carry state)", want, out)
			}
		}
		if _, err := s.call(ctx, "boom", nil, 5*time.Second); err == nil || !strings.Contains(err.Error(), "kaput") {
			t.Fatalf("boom err = %v", err)
		}
	})

	t.Run("timeout ends the wedged instance", func(t *testing.T) {
		s, err := spawnService(ctx, exec.NewOSRunner(), "counter@1.0#t2", res)
		if err != nil {
			t.Fatalf("spawnService: %v", err)
		}
		if _, err := s.call(ctx, "hang", nil, 300*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("hang err = %v", err)
		}
		if !s.dead() {
			t.Fatal("wedged instance survived its timeout")
		}
	})
}

func TestPluginServicesRegistry(t *testing.T) {
	root := installServicePlugin(t)
	res := resolveCounter(t, root)
	ps := newPluginServices(context.Background(), exec.NewOSRunner(), nil)
	defer ps.endAll()

	first, err := ps.acquire("resident/counter@1.0", res, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	again, err := ps.acquire("resident/counter@1.0", res, false)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if again != first || again.id != first.id {
		t.Fatalf("re-acquire = %s, want the shared live instance %s", again.id, first.id)
	}

	fresh, err := ps.acquire("resident/counter@1.0", res, true)
	if err != nil {
		t.Fatalf("fresh acquire: %v", err)
	}
	if fresh.id == first.id {
		t.Fatal("fresh acquire returned the warm instance")
	}
	if !first.dead() {
		t.Fatal("fresh acquire left the replaced instance alive")
	}

	fresh.end()
	respawned, err := ps.acquire("resident/counter@1.0", res, false)
	if err != nil {
		t.Fatalf("respawn acquire: %v", err)
	}
	if respawned.id == fresh.id {
		t.Fatal("acquire returned a dead instance instead of respawning")
	}
}

func TestResolveTurnPluginsServiceBindings(t *testing.T) {
	root := installServicePlugin(t)
	runner := exec.NewOSRunner()
	ps := newPluginServices(context.Background(), runner, nil)
	defer ps.endAll()
	scope := serviceScope{Pipeline: "quakes", Lane: "ingest"}

	t.Run("no supervisor refuses service bindings", func(t *testing.T) {
		_, err := resolveTurnPlugins(root, map[string]declare.PluginUse{
			"c": {Ref: "counter@1.0", Lifetime: declare.LifetimeResident},
		}, runner, nil, nil, scope)
		if err == nil || !strings.Contains(err.Error(), "no service supervisor") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("resident binding shares one instance across turns", func(t *testing.T) {
		use := map[string]declare.PluginUse{"c": {Ref: "counter@1.0", Lifetime: declare.LifetimeResident}}
		one, err := resolveTurnPlugins(root, use, runner, nil, ps, scope)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		defer one.end()
		two, err := resolveTurnPlugins(root, use, runner, nil, ps, scope)
		if err != nil {
			t.Fatalf("second resolve: %v", err)
		}
		defer two.end()
		if one.pins[0].InstanceID == "" || one.pins[0].InstanceID != two.pins[0].InstanceID {
			t.Fatalf("resident pins = %q vs %q, want one shared instance id", one.pins[0].InstanceID, two.pins[0].InstanceID)
		}
		// The shared instance really carries state across the two resolutions.
		r1, err := one.caller.Call(context.Background(), dispatch.TurnCall{Alias: "c", Verb: "count"})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		r2, err := two.caller.Call(context.Background(), dispatch.TurnCall{Alias: "c", Verb: "count"})
		if err != nil {
			t.Fatalf("second call: %v", err)
		}
		if string(r1) != `{"count":1}` || string(r2) != `{"count":2}` {
			t.Fatalf("counts = %s, %s; want 1 then 2 (shared state)", r1, r2)
		}
	})

	t.Run("fresh binding replaces the shared instance", func(t *testing.T) {
		use := map[string]declare.PluginUse{"c": {Ref: "counter@1.0", Lifetime: declare.LifetimeResident, Fresh: true}}
		before, err := ps.acquire("resident/counter@1.0", resolveCounter(t, root), false)
		if err != nil {
			t.Fatalf("prime: %v", err)
		}
		rp, err := resolveTurnPlugins(root, use, runner, nil, ps, scope)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		defer rp.end()
		if rp.pins[0].InstanceID == before.id {
			t.Fatal("fresh binding attached the warm instance")
		}
	})

	t.Run("run binding ends its instance with the turn", func(t *testing.T) {
		use := map[string]declare.PluginUse{"c": {Ref: "counter@1.0"}}
		rp, err := resolveTurnPlugins(root, use, runner, nil, ps, scope)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if rp.pins[0].InstanceID == "" {
			t.Fatal("run-lifetime service pin carries no instance id")
		}
		if _, err := rp.caller.Call(context.Background(), dispatch.TurnCall{Alias: "c", Verb: "count"}); err != nil {
			t.Fatalf("call: %v", err)
		}
		rp.end()
		second, err := resolveTurnPlugins(root, use, runner, nil, ps, scope)
		if err != nil {
			t.Fatalf("second resolve: %v", err)
		}
		defer second.end()
		if second.pins[0].InstanceID == rp.pins[0].InstanceID {
			t.Fatal("run-lifetime instance survived its turn")
		}
	})

	t.Run("lane binding keys on the lane", func(t *testing.T) {
		use := map[string]declare.PluginUse{"c": {Ref: "counter@1.0", Lifetime: declare.LifetimeLane}}
		a, err := resolveTurnPlugins(root, use, runner, nil, ps, serviceScope{Pipeline: "feed", Lane: "etl"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		defer a.end()
		b, err := resolveTurnPlugins(root, use, runner, nil, ps, serviceScope{Pipeline: "report", Lane: "etl"})
		if err != nil {
			t.Fatalf("second resolve: %v", err)
		}
		defer b.end()
		other, err := resolveTurnPlugins(root, use, runner, nil, ps, serviceScope{Pipeline: "solo", Lane: ""})
		if err != nil {
			t.Fatalf("own-lane resolve: %v", err)
		}
		defer other.end()
		if a.pins[0].InstanceID != b.pins[0].InstanceID {
			t.Fatalf("lane members got %q vs %q, want one shared instance", a.pins[0].InstanceID, b.pins[0].InstanceID)
		}
		if other.pins[0].InstanceID == a.pins[0].InstanceID {
			t.Fatal("an own-lane pipeline shared another lane's instance")
		}
	})
}

// fixedCaller answers every call with one canned result.
type fixedCaller struct{ result json.RawMessage }

func (f fixedCaller) Call(context.Context, dispatch.TurnCall) (json.RawMessage, error) {
	return f.result, nil
}

func TestSpillingCaller(t *testing.T) {
	t.Run("small results pass through", func(t *testing.T) {
		s := &spillingCaller{inner: fixedCaller{result: json.RawMessage(`{"ok":1}`)}, dir: t.TempDir()}
		out, err := s.Call(context.Background(), dispatch.TurnCall{Alias: "c", Verb: "v"})
		if err != nil || string(out) != `{"ok":1}` {
			t.Fatalf("= %s, %v", out, err)
		}
	})

	t.Run("oversized results spill content-addressed with digest", func(t *testing.T) {
		big := json.RawMessage(`{"data":"` + strings.Repeat("x", maxPluginResultBytes) + `"}`)
		dir := t.TempDir()
		s := &spillingCaller{inner: fixedCaller{result: big}, dir: dir}
		out, err := s.Call(context.Background(), dispatch.TurnCall{Alias: "c", Verb: "v"})
		if err != nil {
			t.Fatalf("spill: %v", err)
		}
		var reply spilledPayload
		if err := json.Unmarshal(out, &reply); err != nil {
			t.Fatalf("reply not a spill document: %v\n%s", err, out)
		}
		if reply.SHA256 != plugin.Digest(big) || reply.Bytes != int64(len(big)) {
			t.Fatalf("reply = %+v", reply)
		}
		blob, err := os.ReadFile(reply.PayloadPath) //nolint:gosec // test-owned temp path
		if err != nil {
			t.Fatalf("read payload: %v", err)
		}
		if string(blob) != string(big) {
			t.Fatal("payload file differs from the result body")
		}
		if filepath.Dir(reply.PayloadPath) != dir || filepath.Base(reply.PayloadPath) != reply.SHA256+".json" {
			t.Fatalf("payload path %q is not content-addressed under %q", reply.PayloadPath, dir)
		}
	})

	t.Run("no spill directory refuses oversized results", func(t *testing.T) {
		big := json.RawMessage(`{"data":"` + strings.Repeat("x", maxPluginResultBytes) + `"}`)
		s := &spillingCaller{inner: fixedCaller{result: big}}
		if _, err := s.Call(context.Background(), dispatch.TurnCall{Alias: "c", Verb: "v"}); err == nil || !strings.Contains(err.Error(), "no payload directory") {
			t.Fatalf("err = %v", err)
		}
	})
}
