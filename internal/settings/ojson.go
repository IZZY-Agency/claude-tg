package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Object is a JSON object that remembers key order. Values are *Object,
// []any, json.Number, string, bool or nil.
type Object struct {
	Keys   []string
	Values map[string]any
}

func newObject() *Object { return &Object{Values: map[string]any{}} }

// Get returns the value for k.
func (o *Object) Get(k string) (any, bool) { v, ok := o.Values[k]; return v, ok }

// Set stores v under k, appending k to the order when new.
func (o *Object) Set(k string, v any) {
	if _, ok := o.Values[k]; !ok {
		o.Keys = append(o.Keys, k)
	}
	o.Values[k] = v
}

// Delete removes k.
func (o *Object) Delete(k string) {
	if _, ok := o.Values[k]; !ok {
		return
	}
	delete(o.Values, k)
	for i, kk := range o.Keys {
		if kk == k {
			o.Keys = append(o.Keys[:i], o.Keys[i+1:]...)
			break
		}
	}
}

// Parse decodes src (empty input counts as {}) preserving key order and
// number literals.
func Parse(src []byte) (*Object, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return newObject(), nil
	}
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	o, ok := v.(*Object)
	if !ok {
		return nil, fmt.Errorf("settings: top level is not an object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("settings: trailing data after object")
	}
	return o, nil
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := newObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("settings: object key is not a string")
				}
				if _, dup := o.Values[k]; dup {
					return nil, fmt.Errorf("settings: duplicate key %q; refusing to rewrite the file", k)
				}
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return o, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("settings: unexpected delimiter %v", t)
	default:
		return tok, nil // json.Number, string, bool, nil
	}
}

// Marshal renders o with the given indent; newline appends a trailing "\n".
func Marshal(o *Object, indent string, newline bool) ([]byte, error) {
	var compact bytes.Buffer
	if err := writeValue(&compact, o); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact.Bytes(), "", indent); err != nil {
		return nil, err
	}
	if newline {
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func writeValue(w *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case *Object:
		w.WriteByte('{')
		for i, k := range t.Keys {
			if i > 0 {
				w.WriteByte(',')
			}
			writeString(w, k)
			w.WriteByte(':')
			if err := writeValue(w, t.Values[k]); err != nil {
				return err
			}
		}
		w.WriteByte('}')
	case []any:
		w.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				w.WriteByte(',')
			}
			if err := writeValue(w, e); err != nil {
				return err
			}
		}
		w.WriteByte(']')
	case json.Number:
		w.WriteString(string(t))
	case string:
		writeString(w, t)
	case bool:
		if t {
			w.WriteString("true")
		} else {
			w.WriteString("false")
		}
	case nil:
		w.WriteString("null")
	default:
		return fmt.Errorf("settings: unsupported value %T", v)
	}
	return nil
}

func writeString(w *bytes.Buffer, s string) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	w.Write(bytes.TrimRight(b.Bytes(), "\n"))
}

// DetectIndent returns the indentation of the first indented line, defaulting
// to two spaces.
func DetectIndent(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed != line && trimmed != "" {
			return line[:len(line)-len(trimmed)]
		}
	}
	return "  "
}
