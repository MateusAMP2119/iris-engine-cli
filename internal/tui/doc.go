// Package tui is the interactive full-screen surface for Iris (today: `iris ps`).
//
// It owns the live-view model, renderer, terminal session, poller, and last-known
// state cache. The CLI wires cobra and transport into this package; tui must not
// import cli or daemon — only api, config, and leaves.
package tui
