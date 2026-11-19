package main

// Clipboard commands. These are glkvm extensions; stock qemu-ga doesn't have them.
//
//   glkvm-clipboard-get {"since": <seq>}
//       -> {"seq": N, "changed": false}                    when the clipboard sequence is still <seq>
//       -> {"seq": N, "changed": true, "text": "..."|null, "truncated": bool}
//   glkvm-clipboard-set {"text": "..."}
//       -> {"seq": N}    the sequence number after the change, so the KVM
//                        doesn't read its own text straight back
//
// "seq" is Windows' clipboard sequence number: it changes on every clipboard
// update, so polling is cheap. "text" is null when the clipboard holds no text.

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const maxClipboardBytes = 256 * 1024 // UTF-8; fits easily in the KVM's 1 MiB line limit

type Clipboard interface {
	Seq() uint32
	GetText() (text string, ok bool, err error) // ok=false: no text on the clipboard
	SetText(text string) error
}

func registerClipboard(p *Protocol, cb Clipboard) {
	p.Register("glkvm-clipboard-get", func(args json.RawMessage) (any, error) {
		var a struct {
			Since *uint32 `json:"since"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		seq := cb.Seq()
		if a.Since != nil && *a.Since == seq {
			return map[string]any{"seq": seq, "changed": false}, nil
		}
		text, ok, err := cb.GetText()
		if err != nil {
			return nil, genericError("Can't read the clipboard: %v", err)
		}
		result := map[string]any{"seq": seq, "changed": true, "text": nil, "truncated": false}
		if ok {
			text = strings.ReplaceAll(text, "\r\n", "\n")
			text, truncated := truncateUTF8(text, maxClipboardBytes)
			result["text"] = text
			result["truncated"] = truncated
		}
		return result, nil
	})

	p.Register("glkvm-clipboard-set", func(args json.RawMessage) (any, error) {
		var a struct {
			Text *string `json:"text"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		if a.Text == nil {
			return nil, genericError("Parameter 'text' is missing")
		}
		if len(*a.Text) > maxClipboardBytes {
			return nil, genericError("Text is too long (%d bytes, limit %d)", len(*a.Text), maxClipboardBytes)
		}
		// Windows apps expect CRLF line ends in CF_UNICODETEXT
		text := strings.ReplaceAll(strings.ReplaceAll(*a.Text, "\r\n", "\n"), "\n", "\r\n")
		if err := cb.SetText(text); err != nil {
			return nil, genericError("Can't set the clipboard: %v", err)
		}
		return map[string]any{"seq": cb.Seq()}, nil
	})
}

func truncateUTF8(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
