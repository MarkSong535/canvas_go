// Package jsonval works with decoded JSON values (nil, bool, string, json.Number, []any, map[string]any):
// decoding that keeps numbers exactly as written, ordered objects, text for display, and truthiness.
package jsonval

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// OrderedMap is a JSON object that keeps its key order.
type OrderedMap struct {
	Keys   []string
	Values map[string]any
}

func NewOrderedMap() *OrderedMap { return &OrderedMap{Values: map[string]any{}} }

func (m *OrderedMap) Set(key string, value any) {
	if _, ok := m.Values[key]; !ok {
		m.Keys = append(m.Keys, key)
	}
	m.Values[key] = value
}

func (m *OrderedMap) Get(key string) (any, bool) {
	v, ok := m.Values[key]
	return v, ok
}

func (m *OrderedMap) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range m.Keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteByte(':')
		val, err := Marshal(m.Values[k])
		if err != nil {
			return nil, err
		}
		b.Write(val)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// Marshal encodes like json.Marshal but leaves <, > and & as they are (answers and prompts contain HTML).
func Marshal(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(b.String(), "\n")), nil
}

// JSON is Marshal that never fails (an unencodable value becomes its Text).
func JSON(v any) string {
	out, err := Marshal(v)
	if err != nil {
		out, _ = Marshal(Text(v))
	}
	return string(out)
}

// Text is a value as display text: strings as they are, numbers as written, null as "", objects and arrays as
// JSON.
func Text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return string(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return FormatFloat(x)
	case fmt.Stringer:
		return x.String()
	case error:
		return x.Error()
	default:
		return JSON(v)
	}
}

// FormatFloat is the shortest text that reads back as f (93.5, 10, 1e+21).
func FormatFloat(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Truthy reports whether a value is present and non-empty: not null, false, 0, "", [] or {}.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case float64:
		return x != 0
	case int:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case *OrderedMap:
		return len(x.Keys) > 0
	}
	return true
}

// Float converts a JSON number to float64.
func Float(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// Decode decodes one JSON value, keeping numbers as json.Number (their exact text).
func Decode(data []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("extra data after JSON value")
	}
	return v, nil
}
