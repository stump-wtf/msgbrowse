package sentiment

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/store"
)

// salienceFloor discards scores too weak to mean anything. Below it, a score is
// the model hedging rather than reading real evidence, and storing it would pad
// the table (and drag every average toward zero) in proportion to corpus size
// rather than expressive content.
const salienceFloor = 0.2

// errBadResponse marks a response that could not be parsed at all, as opposed
// to a transport error from the LLM call. A bad response is logged and skipped
// rather than aborting the run, so one deterministically-malformed batch cannot
// wedge a conversation forever — the same stance internal/facts takes.
var errBadResponse = errors.New("unparseable sentiment response")

// rawScored is the model's per-message JSON shape.
type rawScored struct {
	Message int                `json:"message"`
	Scores  map[string]float64 `json:"scores"`
}

// parseScores turns a raw model response into store-ready score rows.
//
// Every coercion here is deliberate and required by SPEC-0027 REQ "Anchored
// scoring engine with defensive parsing":
//
//   - code fences and surrounding prose are tolerated (llm.ExtractJSONArray
//     returns the first complete JSON array, so prose brackets cannot splice
//     into the span) rather than failing the batch;
//   - scores are clamped into [-1, +1];
//   - constructs absent from the lexicon are dropped;
//   - a malformed per-message entry is skipped without failing the batch;
//   - scores below the salience floor are discarded before storage.
//
// The one thing it will not do is invent provenance: an out-of-range message
// index is dropped, not clamped to a neighbour, because attributing a score to
// the wrong message is worse than losing it.
func parseScores(raw string, included []store.MessageView, lex *Lexicon, contactFor func(store.MessageView) int64) ([]store.SentimentScore, error) {
	if len(included) == 0 {
		return nil, nil
	}
	body := llm.ExtractJSONArray(raw)
	if body == "" {
		return nil, fmt.Errorf("no JSON array in model response")
	}

	// Unmarshal into json.RawMessage first so ONE malformed element cannot fail
	// the whole batch — the REQ's "a malformed per-message entry is skipped".
	var elems []json.RawMessage
	if err := json.Unmarshal([]byte(body), &elems); err != nil {
		return nil, fmt.Errorf("parse sentiment JSON: %w", err)
	}

	known := make(map[string]struct{}, len(lex.Constructs))
	for _, c := range lex.Constructs {
		known[c.Name] = struct{}{}
	}

	var out []store.SentimentScore
	for _, elem := range elems {
		var rs rawScored
		if err := json.Unmarshal(elem, &rs); err != nil {
			continue // malformed entry: skip it, keep the batch
		}
		idx := rs.Message - 1
		if idx < 0 || idx >= len(included) {
			continue // cites a message that is not in this batch
		}
		msg := included[idx]

		for name, score := range rs.Scores {
			name = strings.TrimSpace(name)
			if _, ok := known[name]; !ok {
				continue // construct outside the lexicon
			}
			if math.IsNaN(score) || math.IsInf(score, 0) {
				continue
			}
			score = math.Max(-1, math.Min(1, score))
			if math.Abs(score) < salienceFloor {
				continue
			}
			out = append(out, store.SentimentScore{
				MessageHash: msg.Hash,
				Construct:   name,
				Score:       score,
				TSUnix:      msg.TSUnix,
				ContactID:   contactFor(msg),
			})
		}
	}
	return out, nil
}
