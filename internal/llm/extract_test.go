package llm

import "testing"

// TestExtractJSONArray folds the former per-package extractor tests (facts,
// sentiment) into one table and adds the prose-bracket cases that the old
// first-'['-to-last-']' heuristic got wrong (#337).
func TestExtractJSONArray(t *testing.T) {
	cases := []struct{ in, want string }{
		{"[1,2]", "[1,2]"},
		{"```json\n[1,2]\n```", "[1,2]"},
		{"sure thing:\n[1,2]\nlet me know", "[1,2]"},
		{"prose [1] more", "[1]"},
		{"[[1,2],[3]]", "[[1,2],[3]]"},
		{"no array here", ""},
		{"]backwards[", ""},
		{"[a]", ""}, // bracketed, but not JSON
		{"[outer [inner] outer]", ""},
		// Prose brackets on either side of the payload used to splice garbage
		// into the span and fail the whole batch.
		{"Here are the scores [as requested]:\n[{\"message\":1}]", "[{\"message\":1}]"},
		{"[{\"message\":1}] — done [as requested]", "[{\"message\":1}]"},
		{"[1,2] and [3,4]", "[1,2]"},
	}
	for _, tc := range cases {
		if got := ExtractJSONArray(tc.in); got != tc.want {
			t.Errorf("ExtractJSONArray(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"summary":"x"}`, `{"summary":"x"}`},
		{"```json\n{\"summary\":\"x\"}\n```", `{"summary":"x"}`},
		{"here you go:\n{\"summary\":\"x\"}\ncheers", `{"summary":"x"}`},
		{"no object here", ""},
		{"}backwards{", ""},
		{"{not json}", ""},
		// Symmetric prose-bracket tolerance on the object side.
		{"Here is the digest {as requested}: {\"summary\":\"x\"}", `{"summary":"x"}`},
		{"{\"summary\":\"x\"} — done {as requested}", `{"summary":"x"}`},
	}
	for _, tc := range cases {
		if got := ExtractJSONObject(tc.in); got != tc.want {
			t.Errorf("ExtractJSONObject(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
