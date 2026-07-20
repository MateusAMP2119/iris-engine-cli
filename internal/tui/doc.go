// Package tui is the interactive full-screen surface for Iris (today: `iris ps`).
//
// It owns the live-view model, renderer, Bubble Tea program, poller, and
// last-known-state cache. The live view rides charmbracelet/bubbletea with
// bubbles/textinput for the COMMANDS palette prompt, bubbles/help for the key
// map, and lipgloss for chrome styles; the pure model + cell-buffer renderer
// stay unit-testable and golden-stable underneath.
//
// The CLI wires cobra and transport into this package; tui must not import cli
// or daemon — only api, config, and leaves.
package tui
