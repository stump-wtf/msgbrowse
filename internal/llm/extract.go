package llm

import (
	"encoding/json"
	"strings"
)

// ExtractJSONArray returns the first complete JSON array embedded in s: the
// span starting at a '[' that decodes as an entire JSON value. Markdown fences
// and prose around the array, including prose that itself contains brackets,
// are tolerated. Returns "" when s holds no decodable array.
func ExtractJSONArray(s string) string {
	return firstJSONValue(s, '[')
}

// ExtractJSONObject is the object twin of ExtractJSONArray: it returns the
// first complete JSON object embedded in s, or "" when there is none.
func ExtractJSONObject(s string) string {
	return firstJSONValue(s, '{')
}

// firstJSONValue scans candidate opening-bracket positions and returns the
// first one that decodes as a complete JSON value. Trusting the first open
// bracket paired with the last close bracket splices prose brackets into the
// span, and the resulting json.Unmarshal failure silently discards a whole
// batch of otherwise fine LLM output. json.Decoder.Decode stops at the end of
// the first value and tolerates trailing content, so InputOffset gives the
// exact end of the value.
func firstJSONValue(s string, open byte) string {
	for from := 0; from < len(s); {
		rel := strings.IndexByte(s[from:], open)
		if rel < 0 {
			return ""
		}
		start := from + rel
		rest := s[start:]
		dec := json.NewDecoder(strings.NewReader(rest))
		var val json.RawMessage
		if err := dec.Decode(&val); err == nil {
			return rest[:dec.InputOffset()]
		}
		from = start + 1
	}
	return ""
}
