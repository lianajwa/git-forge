// json.go — JSON helpers. Node's JSON.stringify preserves object key order and does
// not HTML-escape <, > or &; Go's encoding/json sorts map keys and escapes HTML by
// default. OMap is an insertion-ordered JSON object used wherever key order is
// observable (GraphQL results, MCP tool results), and marshalJSON matches
// JSON.stringify's escaping.

package main

import (
	"bytes"
	"encoding/json"
)

// OMap is a JSON object that preserves insertion order.
type OMap struct {
	keys []string
	vals map[string]any
}

// NewOMap returns an empty insertion-ordered JSON object.
func NewOMap() *OMap {
	return &OMap{vals: make(map[string]any)}
}

// Set stores value under key, appending the key on first use. It returns the map so
// calls chain.
func (o *OMap) Set(key string, value any) *OMap {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
	return o
}

// Get returns the value stored under key.
func (o *OMap) Get(key string) (any, bool) {
	value, ok := o.vals[key]
	return value, ok
}

// Keys returns the keys in insertion order.
func (o *OMap) Keys() []string {
	if o == nil {
		return nil
	}
	out := make([]string, len(o.keys))
	copy(out, o.keys)
	return out
}

// MarshalJSON writes the object with keys in insertion order.
func (o *OMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalJSON(key)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalJSON(o.vals[key])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalJSON encodes a value the way JSON.stringify does: no HTML escaping.
func marshalJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	// Encode appends a newline; JSON.stringify does not.
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

// mustJSON encodes a value, falling back to null on error (a shape the callers never
// hit with the values they build).
func mustJSON(value any) []byte {
	out, err := marshalJSON(value)
	if err != nil {
		return []byte("null")
	}
	return out
}
