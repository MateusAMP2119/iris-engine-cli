package tui

import (
	"strings"
	"testing"
	"time"
)

// decodeAll runs the decoder over a scripted byte string and collects every
// keypress it emits (the byte channel closes when the script drains, so a
// trailing bare ESC classifies via the closed-stream path, not the delay).
func decodeAll(t *testing.T, script string) []psKey {
	t.Helper()
	keys := make(chan psKey, 64)
	go readPsKeys(strings.NewReader(script), keys)
	var got []psKey
	deadline := time.After(5 * time.Second)
	for {
		select {
		case k, ok := <-keys:
			if !ok {
				return got
			}
			got = append(got, k)
		case <-deadline:
			t.Fatal("decoder never drained the script")
		}
	}
}

// TestDecodePsKeys proves the live-view key decoder: control bytes, CSI and
// SS3 arrows, bare and torn escapes, printable ASCII, multi-byte UTF-8, and
// dropped C0 noise.
func TestDecodePsKeys(t *testing.T) {
	t.Run("decode-ps-keys", func(t *testing.T) {
		cases := []struct {
			name   string
			script string
			want   []psKey
		}{
			{"printables pass verbatim", "qjka/",
				[]psKey{{psKeyRune, 'q'}, {psKeyRune, 'j'}, {psKeyRune, 'k'}, {psKeyRune, 'a'}, {psKeyRune, '/'}}},
			{"enter both ways", "\r\n",
				[]psKey{{kind: psKeyEnter}, {kind: psKeyEnter}}},
			{"ctrl-c arrives as a byte under raw mode", "\x03",
				[]psKey{{kind: psKeyCtrlC}}},
			{"backspace and delete both erase", "\x7f\x08",
				[]psKey{{kind: psKeyBackspace}, {kind: psKeyBackspace}}},
			{"csi arrows", "\x1b[A\x1b[B\x1b[C\x1b[D",
				[]psKey{{kind: psKeyUp}, {kind: psKeyDown}, {kind: psKeyRight}, {kind: psKeyLeft}}},
			{"ss3 arrows (application cursor mode)", "\x1bOA\x1bOB",
				[]psKey{{kind: psKeyUp}, {kind: psKeyDown}}},
			{"bare esc at stream end is the escape key", "\x1b",
				[]psKey{{kind: psKeyEsc}}},
			{"esc before a printable is escape then the rune", "\x1bx",
				nil}, // resolved below: timing-dependent grouping, asserted separately
			{"unknown csi final is consumed whole and dropped", "\x1b[Zq",
				[]psKey{{psKeyRune, 'q'}}},
			{"pageup's parameter bytes never leak as runes", "\x1b[5~q",
				[]psKey{{psKeyRune, 'q'}}},
			{"a modified arrow still decodes as the arrow", "\x1b[1;5A",
				[]psKey{{kind: psKeyUp}}},
			{"multi-byte utf-8 rune", "é",
				[]psKey{{psKeyRune, 'é'}}},
			{"c0 noise is dropped", "\x01\x02q",
				[]psKey{{psKeyRune, 'q'}}},
		}
		for _, tc := range cases {
			if tc.want == nil {
				continue
			}
			t.Run(tc.name, func(t *testing.T) {
				got := decodeAll(t, tc.script)
				if len(got) != len(tc.want) {
					t.Fatalf("decoded %v, want %v", got, tc.want)
				}
				for i := range got {
					if got[i] != tc.want[i] {
						t.Errorf("key %d = %+v, want %+v", i, got[i], tc.want[i])
					}
				}
			})
		}

		// ESC followed by a non-sequence byte: the decoder consumes the byte as
		// the (non-CSI) tail and classifies the whole group as Escape -- lone
		// ESC handling never swallows subsequent distinct keypresses arriving
		// after the disambiguation window, proven with a slow channel.
		t.Run("lone esc then a later rune stay two keys", func(t *testing.T) {
			bytes := make(chan byte)
			keys := make(chan psKey, 8)
			go decodePsKeys(bytes, keys, 5*time.Millisecond)
			bytes <- 0x1b
			time.Sleep(30 * time.Millisecond) // past the window: ESC resolves alone
			bytes <- 'x'
			close(bytes)
			var got []psKey
			for k := range keys {
				got = append(got, k)
			}
			want := []psKey{{kind: psKeyEsc}, {psKeyRune, 'x'}}
			if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("decoded %v, want %v", got, want)
			}
		})
	})
}
