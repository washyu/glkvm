package main

// The qemu-ga wire protocol, as much of it as the KVM uses.
//
//   - Requests are JSON objects {"execute": "<command>", "arguments": {...}},
//     one per line.
//   - Replies are {"return": ...} or {"error": {"class": ..., "desc": ...}},
//     each followed by a newline.
//   - A 0xFF byte from the KVM resets the parser (drops any partial request).
//   - guest-sync-delimited replies are prefixed with 0xFF so the KVM can find
//     the start of the reply among stale output.
//   - guest-shutdown sends nothing back on success.
//
// This file has no Windows dependencies so it can be tested anywhere.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

const maxRequestLine = 2 * 1024 * 1024 // Bigger than any clipboard we accept

// CommandError becomes a qemu-ga {"error": ...} reply.
type CommandError struct {
	Class string
	Desc  string
}

func (e *CommandError) Error() string { return e.Class + ": " + e.Desc }

func genericError(format string, args ...any) *CommandError {
	return &CommandError{Class: "GenericError", Desc: fmt.Sprintf(format, args...)}
}

// noReply is returned by handlers that must not answer on success (guest-shutdown).
type noReply struct{}

// delimited marks a reply that must be prefixed with 0xFF (guest-sync-delimited).
type delimited struct{ value any }

type Handler func(args json.RawMessage) (any, error)

type Protocol struct {
	Version  string
	handlers map[string]Handler
	buf      []byte
	skipLine bool // Dropping the rest of an oversized line
}

func NewProtocol(version string) *Protocol {
	p := &Protocol{Version: version, handlers: map[string]Handler{}}
	p.Register("guest-sync", func(args json.RawMessage) (any, error) {
		return syncID(args)
	})
	p.Register("guest-sync-delimited", func(args json.RawMessage) (any, error) {
		id, err := syncID(args)
		if err != nil {
			return nil, err
		}
		return delimited{id}, nil
	})
	p.Register("guest-ping", func(json.RawMessage) (any, error) {
		return struct{}{}, nil
	})
	p.Register("guest-info", func(json.RawMessage) (any, error) {
		return p.info(), nil
	})
	return p
}

func (p *Protocol) Register(name string, h Handler) {
	p.handlers[name] = h
}

func (p *Protocol) Commands() []string {
	names := make([]string, 0, len(p.handlers))
	for name := range p.handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (p *Protocol) info() any {
	type cmdInfo struct {
		Name            string `json:"name"`
		Enabled         bool   `json:"enabled"`
		SuccessResponse bool   `json:"success-response"`
	}
	cmds := []cmdInfo{}
	for _, name := range p.Commands() {
		cmds = append(cmds, cmdInfo{Name: name, Enabled: true, SuccessResponse: name != "guest-shutdown"})
	}
	return map[string]any{"version": p.Version, "supported_commands": cmds}
}

// Reset drops any partial request, e.g. after the port was reopened.
func (p *Protocol) Reset() {
	p.buf = p.buf[:0]
	p.skipLine = false
}

// Feed takes raw bytes from the port and returns the bytes to write back.
func (p *Protocol) Feed(data []byte) []byte {
	var out bytes.Buffer
	for len(data) > 0 {
		// 0xFF never appears in valid UTF-8 JSON: it's always a parser reset
		if i := bytes.IndexByte(data, 0xFF); i == 0 {
			p.Reset()
			data = data[1:]
			continue
		} else if i > 0 {
			p.consume(data[:i], &out)
			data = data[i:]
			continue
		}
		p.consume(data, &out)
		break
	}
	return out.Bytes()
}

func (p *Protocol) consume(data []byte, out *bytes.Buffer) {
	for len(data) > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			if !p.skipLine {
				p.buf = append(p.buf, data...)
				if len(p.buf) > maxRequestLine {
					p.buf = p.buf[:0]
					p.skipLine = true
					out.Write(encodeReply(nil, genericError("Request too long")))
				}
			}
			return
		}
		if p.skipLine {
			p.skipLine = false
		} else {
			p.buf = append(p.buf, data[:nl]...)
			if line := bytes.TrimSpace(p.buf); len(line) > 0 {
				out.Write(p.handleLine(line))
			}
		}
		p.buf = p.buf[:0]
		data = data[nl+1:]
	}
}

func (p *Protocol) handleLine(line []byte) []byte {
	var req struct {
		Execute   string          `json:"execute"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return encodeReply(nil, genericError("Invalid JSON: %v", err))
	}
	if req.Execute == "" {
		return encodeReply(nil, genericError("Missing 'execute'"))
	}
	h, ok := p.handlers[req.Execute]
	if !ok {
		return encodeReply(nil, &CommandError{Class: "CommandNotFound", Desc: fmt.Sprintf("The command %s has not been found", req.Execute)})
	}
	result, err := safeCall(h, req.Arguments)
	if err != nil {
		return encodeReply(nil, err)
	}
	switch r := result.(type) {
	case noReply:
		return nil
	case delimited:
		return append([]byte{0xFF}, encodeReply(r.value, nil)...)
	}
	return encodeReply(result, nil)
}

func safeCall(h Handler, args json.RawMessage) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = genericError("Internal error: %v", r)
		}
	}()
	return h(args)
}

func encodeReply(result any, err error) []byte {
	var msg any
	if err != nil {
		ce, ok := err.(*CommandError)
		if !ok {
			ce = genericError("%v", err)
		}
		msg = map[string]any{"error": map[string]string{"class": ce.Class, "desc": ce.Desc}}
	} else {
		if result == nil {
			result = struct{}{}
		}
		msg = map[string]any{"return": result}
	}
	data, mErr := json.Marshal(msg)
	if mErr != nil {
		data, _ = json.Marshal(map[string]any{"error": map[string]string{"class": "GenericError", "desc": mErr.Error()}})
	}
	return append(data, '\n')
}

func syncID(args json.RawMessage) (any, error) {
	var a struct {
		ID *json.Number `json:"id"`
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if len(args) == 0 || dec.Decode(&a) != nil || a.ID == nil {
		return nil, genericError("Parameter 'id' is missing")
	}
	return *a.ID, nil
}

// decodeArgs unmarshals command arguments, treating a missing object as empty.
func decodeArgs(args json.RawMessage, into any) error {
	if len(args) == 0 || string(args) == "null" {
		return nil
	}
	if err := json.Unmarshal(args, into); err != nil {
		return genericError("Invalid arguments: %v", err)
	}
	return nil
}
