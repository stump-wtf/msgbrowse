// Package sentiment builds the IPIP-anchored lexicon that the scoring engine
// prompts with, on top of the public-domain IPIP item-assignment table shipped
// alongside this file.
//
// The split is deliberate (ADR-0028 §1, §5): the full item table is *data* —
// 3,805 public-domain assignments, embedded verbatim so it stays attributable
// and diffable — while the curation that selects ~15 scoring constructs out of
// its 246 labels is *code*, so it is type-checked, testable, and needs no
// runtime config parsing.
package sentiment

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	_ "embed"
)

//go:embed ipip_items.csv
var ipipCSV string

// Item is one row of the IPIP item-assignment table: a single item assigned to
// a single construct label by a single instrument.
type Item struct {
	// Instrument is the inventory the assignment comes from (e.g. "16PF").
	Instrument string
	// Alpha is the source scale's reliability. Where the table lists more than
	// one alpha for a row (an item reached via several scales), this is the
	// highest — the design's "prefer the highest-alpha assignment" rule. It is
	// 0 where the source reports no usable reliability (see parseAlpha), which
	// keeps those rows below any anchor threshold: an assignment with no
	// reported reliability is not a marker item.
	Alpha float64
	// Key is the keying direction: +1 positively keyed, -1 negatively keyed.
	// The table leaves it 0 on 39 VIA rows where the direction is undetermined;
	// those rows are loaded faithfully but are never eligible as anchors,
	// because an anchor with no direction cannot orient the model.
	Key int
	// Text is the item itself ("Act wild and crazy.").
	Text string
	// Label is the construct the item is assigned to ("Gregariousness").
	Label string
}

// loadItems parses the embedded table. It is wrapped in sync.OnceValues because
// every scoring run builds the lexicon once and the table never changes at
// runtime.
var loadItems = sync.OnceValues(func() ([]Item, error) {
	return parseItems(strings.NewReader(ipipCSV))
})

// Items returns the embedded IPIP item-assignment table.
func Items() ([]Item, error) { return loadItems() }

// parseItems reads the normalized CSV. Lines beginning with '#' are the
// attribution and provenance header; encoding/csv is configured to skip them.
func parseItems(r io.Reader) ([]Item, error) {
	cr := csv.NewReader(r)
	cr.Comment = '#'
	cr.FieldsPerRecord = 5

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("reading ipip item table header: %w", err)
	}
	if want := []string{"instrument", "alpha", "key", "text", "label"}; !equalStrings(header, want) {
		return nil, fmt.Errorf("ipip item table header = %v, want %v", header, want)
	}

	var out []Item
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading ipip item table: %w", err)
		}
		alpha, err := parseAlpha(rec[1])
		if err != nil {
			return nil, fmt.Errorf("ipip item %q (%s): %w", rec[3], rec[4], err)
		}
		key, err := strconv.Atoi(rec[2])
		if err != nil {
			return nil, fmt.Errorf("ipip item %q (%s): parsing key %q: %w", rec[3], rec[4], rec[2], err)
		}
		if key != 1 && key != -1 && key != 0 {
			return nil, fmt.Errorf("ipip item %q (%s): key = %d, want +1, -1 or 0", rec[3], rec[4], key)
		}
		out = append(out, Item{
			Instrument: rec[0],
			Alpha:      alpha,
			Key:        key,
			Text:       rec[3],
			Label:      rec[4],
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ipip item table is empty")
	}
	return out, nil
}

// parseAlpha reads the table's alpha field. Four shapes occur:
//
//   - a single number ("0.78") — the common case;
//   - a comma-separated list ("0.59,0.63") on 278 rows where the item is
//     assigned via more than one scale, where the highest value wins, matching
//     the anchor-selection rule;
//   - "." on 32 IPIP-IPC rows, meaning the source reports no alpha;
//   - a value outside [0,1] on 26 rows — 16 BIS_BAS rows carrying "1994" (the
//     Carver & White citation year) and 10 MPQ rows carrying "12" (an item
//     count). Cronbach's alpha is bounded above by 1, so these are data-entry
//     slips in the source table, not reliabilities.
//
// The last two both yield 0, meaning "no reported reliability". Returning 0
// rather than an error keeps one bad cell from failing the whole table, and —
// more importantly — keeps those rows below minAnchorAlpha. Passing "1994"
// through would rank a mis-keyed cell above every genuine anchor, which is
// exactly what it did before this was caught: six Anxiety assignments sorted
// ahead of the real markers purely because 1994 > 0.99.
func parseAlpha(s string) (float64, error) {
	best := 0.0
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" || part == "." {
			continue
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing alpha %q: %w", s, err)
		}
		if v < 0 || v > 1 {
			continue // not a reliability coefficient; see the doc comment
		}
		if v > best {
			best = v
		}
	}
	return best, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
