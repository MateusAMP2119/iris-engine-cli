package dispatch

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The turn protocol model is pure, so these tests drive the codec and the
// collector's frame discipline with literal wire lines: every violation class
// the epic names (unparseable line, row outside declared writes, frame after
// terminal, wrong turn echo) and the two terminal shapes.

// testWrites is the declared-writes surface the collector tests enforce against.
func testWrites() WriteSet {
	return WriteSet{"marts.daily": {"day": true, "sum": true}}
}

func TestEncodeFrames(t *testing.T) {
	if got := EncodeGoFrame(841); got != `{"event":"go","turn":841}` {
		t.Fatalf("go frame: %s", got)
	}
	if got := EncodeRunFrame(); got != `{"event":"run"}` {
		t.Fatalf("run frame: %s", got)
	}
	row, err := EncodeRowFrame("raw.orders", json.RawMessage(`{"id":9,"total":40}`))
	if err != nil {
		t.Fatalf("encode row frame: %v", err)
	}
	if row != `{"event":"row","table":"raw.orders","row":{"id":9,"total":40}}` {
		t.Fatalf("row frame: %s", row)
	}
	// Every engine frame must be one line: the protocol is JSON Lines.
	for _, f := range []string{EncodeGoFrame(1), EncodeRunFrame(), row} {
		if strings.ContainsRune(f, '\n') {
			t.Fatalf("frame is not one line: %q", f)
		}
	}
}

func TestTurnCollectorDone(t *testing.T) {
	c := NewTurnCollector(841, testWrites())
	if _, terminal, err := c.Feed(`{"event":"row","table":"marts.daily","row":{"day":"2026-07-17","sum":52}}`); err != nil || terminal {
		t.Fatalf("row feed: terminal=%v err=%v", terminal, err)
	}
	end, terminal, err := c.Feed(`{"event":"done","turn":841}`)
	if err != nil || !terminal || end.Errored {
		t.Fatalf("done feed: end=%+v terminal=%v err=%v", end, terminal, err)
	}
	rows := c.Rows()
	if len(rows) != 1 || rows[0].Table != "marts.daily" || string(rows[0].Row) != `{"day":"2026-07-17","sum":52}` {
		t.Fatalf("collected rows: %+v", rows)
	}
}

func TestTurnCollectorErrorTerminal(t *testing.T) {
	c := NewTurnCollector(7, testWrites())
	end, terminal, err := c.Feed(`{"event":"error","turn":7,"reason":"upstream gone","detail":{"code":3}}`)
	if err != nil || !terminal {
		t.Fatalf("error feed: terminal=%v err=%v", terminal, err)
	}
	if !end.Errored || end.Reason != "upstream gone" || string(end.Detail) != `{"code":3}` {
		t.Fatalf("error end: %+v", end)
	}
}

func TestTurnCollectorErrorReasonDefaults(t *testing.T) {
	c := NewTurnCollector(7, testWrites())
	end, terminal, err := c.Feed(`{"event":"error","turn":7}`)
	if err != nil || !terminal || !end.Errored || end.Reason == "" {
		t.Fatalf("bare error feed: end=%+v terminal=%v err=%v", end, terminal, err)
	}
}

func TestTurnCollectorViolations(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"unparseable", `done 0`},
		{"unknown event", `{"event":"bark"}`},
		{"undeclared table", `{"event":"row","table":"raw.other","row":{"day":"x"}}`},
		{"undeclared field", `{"event":"row","table":"marts.daily","row":{"day":"x","hacked":1}}`},
		{"row not an object", `{"event":"row","table":"marts.daily","row":[1,2]}`},
		{"empty row object", `{"event":"row","table":"marts.daily","row":{}}`},
		{"row without table", `{"event":"row","row":{"day":"x"}}`},
		{"wrong turn echo", `{"event":"done","turn":840}`},
		{"terminal without turn", `{"event":"done"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewTurnCollector(841, testWrites())
			_, terminal, err := c.Feed(tc.line)
			if terminal {
				t.Fatalf("violation reported terminal")
			}
			var fe *FrameError
			if !errors.As(err, &fe) {
				t.Fatalf("want *FrameError, got %v", err)
			}
			if fe.Line != tc.line {
				t.Fatalf("violation does not quote the offending line: %q", fe.Line)
			}
		})
	}
}

func TestTurnCollectorFrameAfterTerminal(t *testing.T) {
	c := NewTurnCollector(3, testWrites())
	if _, terminal, err := c.Feed(`{"event":"done","turn":3}`); err != nil || !terminal {
		t.Fatalf("done feed: terminal=%v err=%v", terminal, err)
	}
	var fe *FrameError
	if _, _, err := c.Feed(`{"event":"done","turn":3}`); !errors.As(err, &fe) {
		t.Fatalf("frame after terminal: want *FrameError, got %v", err)
	}
}
