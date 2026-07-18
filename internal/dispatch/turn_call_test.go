package dispatch

import (
	"errors"
	"strings"
	"testing"
)

// The #215 call legs: declared calls collect, everything else violates.

func testCalls() CallSet {
	return CallSet{"mail": {"send": true}}
}

func TestCollectorAdmitsDeclaredCall(t *testing.T) {
	c := NewTurnCollector(9, testWrites(), testCalls())
	_, call, terminal, err := c.Feed(`{"event":"call","call":1,"verb":"mail.send","args":{"to":"x@y.z"}}`)
	if err != nil || terminal {
		t.Fatalf("declared call: err %v, terminal %v", err, terminal)
	}
	if call == nil || call.Call != 1 || call.Alias != "mail" || call.Verb != "send" || string(call.Args) != `{"to":"x@y.z"}` {
		t.Fatalf("call = %+v", call)
	}
	// A second call before the reply violates.
	if _, _, _, err := c.Feed(`{"event":"call","call":2,"verb":"mail.send"}`); err == nil || !strings.Contains(err.Error(), "before call 1's reply") {
		t.Fatalf("overlapping call err = %v", err)
	}
	// After the reply, the next call is admissible again and the turn can end.
	c.ReplyDelivered()
	if _, call, _, err := c.Feed(`{"event":"call","call":2,"verb":"mail.send"}`); err != nil || call == nil {
		t.Fatalf("post-reply call: %v %+v", err, call)
	}
	c.ReplyDelivered()
	if _, _, terminal, err := c.Feed(`{"event":"done","turn":9}`); err != nil || !terminal {
		t.Fatalf("terminal after calls: err %v, terminal %v", err, terminal)
	}
}

func TestCollectorCallViolations(t *testing.T) {
	tests := []struct {
		name, line, want string
	}{
		{"no call id", `{"event":"call","verb":"mail.send"}`, "no call id"},
		{"verb not alias.verb", `{"event":"call","call":1,"verb":"send"}`, "not alias.verb"},
		{"undeclared alias", `{"event":"call","call":1,"verb":"browser.fetch"}`, `alias "browser" is not in the declared plugins`},
		{"undeclared verb", `{"event":"call","call":1,"verb":"mail.burn"}`, `verb "burn" is not declared by plugin "mail"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewTurnCollector(9, testWrites(), testCalls())
			_, _, _, err := c.Feed(tc.line)
			var fe *FrameError
			if !errors.As(err, &fe) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want violation containing %q", err, tc.want)
			}
		})
	}
}

func TestCollectorNilCallSetRefusesCalls(t *testing.T) {
	c := NewTurnCollector(9, testWrites(), nil)
	if _, _, _, err := c.Feed(`{"event":"call","call":1,"verb":"mail.send"}`); err == nil {
		t.Fatal("nil call set admitted a call")
	}
}

func TestEncodeResFrames(t *testing.T) {
	ok, err := EncodeResOK(3, []byte(`{"message_id":"m1"}`))
	if err != nil || ok != `{"event":"res","call":3,"ok":true,"result":{"message_id":"m1"}}` {
		t.Fatalf("EncodeResOK = %q, %v", ok, err)
	}
	er, err := EncodeResErr(4, "timeout")
	if err != nil || er != `{"event":"res","call":4,"ok":false,"error":"timeout"}` {
		t.Fatalf("EncodeResErr = %q, %v", er, err)
	}
}
