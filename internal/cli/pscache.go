package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// This file is the ps view's last-known-state cache: one small JSON document
// per engine target under the engine home, refreshed by the live view's
// once-a-minute history polls. It exists for exactly one moment: `iris ps`
// against an unreachable engine, which opens the cached snapshot read-only
// (with a standing banner) instead of tearing down -- the docker-parity
// behavior the view pairs with the poller's reconnect loop. It is display
// state, not truth: the machine surface (--json) never touches it, and a
// reconnect replaces it wholesale. Log tails are deliberately not cached.

// psCacheDir is the cache directory name under the engine home.
const psCacheDir = "ps-cache"

// psCacheDoc is the cached document: the target it belongs to (a hash
// collision guard), the save moment (client-local, display-only: it sizes the
// "cached Xm ago" banner), and the last good payload and listing.
type psCacheDoc struct {
	Target      string                 `json:"target"`
	SavedAtUnix int64                  `json:"saved_at_unix"`
	Ps          api.PsPayload          `json:"ps"`
	Pipelines   []api.PipelineListItem `json:"pipelines"`
}

// psCache is the cache handle for one resolved target. A nil handle (the
// engine home could not resolve) drops every save and misses every load --
// the cache is best-effort by contract, never a fault source.
type psCache struct {
	target string
	path   string
}

// newPsCache resolves the cache handle for a target (engineAddr's rendering).
func newPsCache(target string) *psCache {
	home, err := config.Home(os.Getenv)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256([]byte(target))
	return &psCache{
		target: target,
		path:   filepath.Join(home, psCacheDir, hex.EncodeToString(sum[:8])+".json"),
	}
}

// save persists a snapshot as the target's last known state: best-effort,
// atomic (temp file + rename), owner-only permissions, log tail dropped. A
// failed save is silently skipped -- the cache must never make a healthy view
// noisy.
func (c *psCache) save(snap psSnapshot) {
	if c == nil {
		return
	}
	doc := psCacheDoc{
		Target:      c.target,
		SavedAtUnix: time.Now().Unix(),
		Ps:          snap.ps,
		Pipelines:   snap.pipelines,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, c.path)
}

// load reads the target's last known state back: the snapshot, its save
// moment, and whether a usable document existed. A missing, unreadable, or
// wrong-target document is a miss, never an error.
func (c *psCache) load() (psSnapshot, time.Time, bool) {
	if c == nil {
		return psSnapshot{}, time.Time{}, false
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return psSnapshot{}, time.Time{}, false
	}
	var doc psCacheDoc
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Target != c.target {
		return psSnapshot{}, time.Time{}, false
	}
	return psSnapshot{ps: doc.Ps, pipelines: doc.Pipelines}, time.Unix(doc.SavedAtUnix, 0), true
}
