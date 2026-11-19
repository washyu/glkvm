package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type fakeClipboard struct {
	seq  uint32
	text string
	has  bool
}

func (f *fakeClipboard) Seq() uint32 { return f.seq }
func (f *fakeClipboard) GetText() (string, bool, error) {
	return f.text, f.has, nil
}
func (f *fakeClipboard) SetText(text string) error {
	f.seq += 2 // Windows bumps it for EmptyClipboard and SetClipboardData
	f.text, f.has = text, true
	return nil
}

func newTestProtocol() (*Protocol, *fakeClipboard) {
	p := NewProtocol("test")
	cb := &fakeClipboard{seq: 10, text: "hello\r\nworld", has: true}
	registerClipboard(p, cb)
	p.Register("guest-shutdown", func(json.RawMessage) (any, error) { return noReply{}, nil })
	return p, cb
}

func decode(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimRight(line, "\n"), &m); err != nil {
		t.Fatalf("bad reply %q: %v", line, err)
	}
	return m
}

func TestSyncDelimited(t *testing.T) {
	p, _ := newTestProtocol()
	out := p.Feed([]byte("\xff{\"execute\":\"guest-sync-delimited\",\"arguments\":{\"id\":123456789012}}\n"))
	if len(out) == 0 || out[0] != 0xFF {
		t.Fatalf("missing 0xFF prefix: %q", out)
	}
	if got := string(out[1:]); got != "{\"return\":123456789012}\n" {
		t.Fatalf("got %q", got)
	}
}

func TestResetDropsPartial(t *testing.T) {
	p, _ := newTestProtocol()
	if out := p.Feed([]byte(`{"execute":"guest-pi`)); len(out) != 0 {
		t.Fatalf("unexpected output %q", out)
	}
	out := p.Feed([]byte("\xff{\"execute\":\"guest-ping\"}\n"))
	if string(out) != "{\"return\":{}}\n" {
		t.Fatalf("got %q", out)
	}
}

func TestSplitAndMultipleRequests(t *testing.T) {
	p, _ := newTestProtocol()
	var out []byte
	for _, chunk := range []string{`{"execute":`, `"guest-ping"}` + "\n" + `{"execute":"nope"}`, "\n"} {
		out = append(out, p.Feed([]byte(chunk))...)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 || lines[0] != `{"return":{}}` {
		t.Fatalf("got %q", out)
	}
	if e := decode(t, []byte(lines[1]))["error"].(map[string]any); e["class"] != "CommandNotFound" {
		t.Fatalf("got %v", e)
	}
}

func TestInfo(t *testing.T) {
	p, _ := newTestProtocol()
	r := decode(t, p.Feed([]byte("{\"execute\":\"guest-info\"}\n")))["return"].(map[string]any)
	names := map[string]bool{}
	for _, c := range r["supported_commands"].([]any) {
		m := c.(map[string]any)
		names[m["name"].(string)] = m["enabled"].(bool)
		if m["name"] == "guest-shutdown" && m["success-response"] != false {
			t.Fatal("guest-shutdown must not claim a success response")
		}
	}
	for _, want := range []string{"guest-ping", "guest-sync-delimited", "glkvm-clipboard-get", "glkvm-clipboard-set"} {
		if !names[want] {
			t.Fatalf("%s missing from %v", want, names)
		}
	}
}

func TestShutdownNoReply(t *testing.T) {
	p, _ := newTestProtocol()
	if out := p.Feed([]byte("{\"execute\":\"guest-shutdown\",\"arguments\":{\"mode\":\"reboot\"}}\n")); len(out) != 0 {
		t.Fatalf("got %q", out)
	}
}

func TestBadJSON(t *testing.T) {
	p, _ := newTestProtocol()
	if e := decode(t, p.Feed([]byte("{nope\n")))["error"].(map[string]any); e["class"] != "GenericError" {
		t.Fatalf("got %v", e)
	}
}

func TestClipboard(t *testing.T) {
	p, cb := newTestProtocol()
	r := decode(t, p.Feed([]byte("{\"execute\":\"glkvm-clipboard-get\"}\n")))["return"].(map[string]any)
	if r["changed"] != true || r["text"] != "hello\nworld" || r["seq"].(float64) != 10 {
		t.Fatalf("got %v", r)
	}
	r = decode(t, p.Feed([]byte("{\"execute\":\"glkvm-clipboard-get\",\"arguments\":{\"since\":10}}\n")))["return"].(map[string]any)
	if r["changed"] != false || r["text"] != nil {
		t.Fatalf("got %v", r)
	}
	r = decode(t, p.Feed([]byte("{\"execute\":\"glkvm-clipboard-set\",\"arguments\":{\"text\":\"a\\nb ✓\"}}\n")))["return"].(map[string]any)
	if r["seq"].(float64) != 12 || cb.text != "a\r\nb ✓" {
		t.Fatalf("got %v / %q", r, cb.text)
	}
	cb.has = false
	cb.seq++
	r = decode(t, p.Feed([]byte("{\"execute\":\"glkvm-clipboard-get\",\"arguments\":{\"since\":12}}\n")))["return"].(map[string]any)
	if r["changed"] != true || r["text"] != nil {
		t.Fatalf("got %v", r)
	}
}

func TestClipboardTooLong(t *testing.T) {
	p, _ := newTestProtocol()
	big, _ := json.Marshal(map[string]any{"execute": "glkvm-clipboard-set", "arguments": map[string]string{"text": strings.Repeat("x", maxClipboardBytes+1)}})
	if _, ok := decode(t, p.Feed(append(big, '\n')))["error"]; !ok {
		t.Fatal("expected an error")
	}
}

func TestTruncateUTF8(t *testing.T) {
	s, cut := truncateUTF8("ab✓", 3) // ✓ is 3 bytes; must not split it
	if s != "ab" || !cut {
		t.Fatalf("got %q %v", s, cut)
	}
}

func TestOversizedLineRecovers(t *testing.T) {
	p, _ := newTestProtocol()
	out := p.Feed(bytes.Repeat([]byte("x"), maxRequestLine+10))
	if _, ok := decode(t, out)["error"]; !ok {
		t.Fatalf("got %q", out)
	}
	out = p.Feed([]byte("more garbage\n{\"execute\":\"guest-ping\"}\n"))
	if string(out) != "{\"return\":{}}\n" {
		t.Fatalf("got %q", out)
	}
}
