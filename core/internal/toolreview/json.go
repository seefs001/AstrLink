package toolreview

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// exactFieldNames rejects encoding/json's case-insensitive struct-field aliases
// before typed decoding. Only schema fields are checked. RawMessage boundaries
// preserve user arguments and other opaque data, including case-distinct keys.
func exactFieldNames(data []byte, schema reflect.Type) bool {
	if schema == nil {
		return false
	}
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	if schema == reflect.TypeFor[json.RawMessage]() {
		return true
	}
	switch schema.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return false
		}
		for key, value := range fields {
			for i := 0; i < schema.NumField(); i++ {
				field := schema.Field(i)
				if !field.IsExported() {
					continue
				}
				name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
				if name == "-" {
					continue
				}
				if name == "" {
					name = field.Name
				}
				if key == name {
					if !exactFieldNames(value, field.Type) {
						return false
					}
					break
				}
				if strings.EqualFold(key, name) {
					return false
				}
			}
		}
	case reflect.Slice, reflect.Array:
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return false
		}
		for _, item := range items {
			if !exactFieldNames(item, schema.Elem()) {
				return false
			}
		}
	case reflect.Map:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return false
		}
		for _, value := range fields {
			if !exactFieldNames(value, schema.Elem()) {
				return false
			}
		}
	}
	return true
}

// Reject ambiguous duplicate keys instead of letting the collector, reviewer,
// and eventual tool executor disagree about which value is authoritative.
func strictJSON(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if !jsonValue(decoder, 0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func jsonValue(decoder *json.Decoder, depth int) bool {
	if depth > 128 {
		return false
	}
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return false
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return false
			}
			seen[name] = true
			if !jsonValue(decoder, depth+1) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for decoder.More() {
			if !jsonValue(decoder, depth+1) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim(']')
	default:
		return false
	}
}
