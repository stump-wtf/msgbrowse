package web

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// The full-page /search is the no-JS / progressive-enhancement path: the form's
// plain GET submission lands here and the results must be server-rendered,
// while HTMX replaces the same markup from /search/results. Both handlers share
// runSearch, so this file pins the contract that matters — the no-JS page shows
// the same matches, in the same order, with the same affordances as the partial,
// for every filter the form offers.

// resultCardHrefs pulls the rendered result cards in document order. A card's
// href is conversation + message + anchor, so the sequence is the whole result
// contract: which messages matched, and in what order.
var resultCardHrefs = regexp.MustCompile(`class="result-card[^"]*"\s+href="([^"]+)"`)

func cardSequence(body string) []string {
	var out []string
	for _, m := range resultCardHrefs.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// seedFilterLab writes a conversation whose four messages are separated by
// exactly one filter each, so every case below can name the messages a filter
// must and must not return. The term "fluvial" appears nowhere else in the
// fixture archive, so a count is a statement about the filter, not the seed.
func seedFilterLab(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, "Filter Lab")
	if err != nil {
		t.Fatal(err)
	}
	msg := func(ts, sender, body string, atts []signal.Attachment, links []signal.Link) signal.Message {
		parsed, err := time.Parse(signal.TimestampLayout, ts)
		if err != nil {
			t.Fatal(err)
		}
		return signal.Message{Conversation: "Filter Lab", Timestamp: parsed, TimestampRaw: ts,
			Sender: sender, Body: body, Attachments: atts, Links: links}
	}
	msgs := []signal.Message{
		msg("2022-03-01 09:00:00", "Ada", "fluvial alpha",
			[]signal.Attachment{{Kind: signal.KindImage, RelPath: "media/plate.heic", OriginalName: "core sample"}}, nil),
		msg("2022-03-15 09:00:00", "Ada", "fluvial beta", nil,
			[]signal.Link{{URL: "https://example.com/beta"}}),
		msg("2022-04-02 09:00:00", "Zed", "fluvial gamma", nil, nil),
		msg("2022-04-20 09:00:00", "Zed", "fluvial delta",
			[]signal.Attachment{{Kind: signal.KindImage, RelPath: "media/delta.heic", OriginalName: "outcrop map"}},
			[]signal.Link{{URL: "https://example.com/delta"}}),
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestSearchFullPageRendersResultsWithoutJS: a plain GET with no HX-Request
// header must return a complete document whose results are already rendered —
// the search page has to work in a browser with JavaScript switched off.
func TestSearchFullPageRendersResultsWithoutJS(t *testing.T) {
	srv, st, _ := newTestServer(t)
	lab := seedFilterLab(t, st)

	rec := get(t, srv, "/search?q=fluvial")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "<!doctype html>") {
		t.Error("no-JS search is not a complete document")
	}
	if !contains(body, `<form action="/search" method="get"`) {
		t.Error(`search form lost its plain GET submission (action="/search" method="get"), the no-JS path`)
	}
	if !contains(body, `name="q" value="fluvial"`) {
		t.Error("query is not echoed back into the form")
	}
	if !contains(body, "4 results") {
		t.Errorf("results meta does not report 4 results:\n%.300q", body)
	}
	for _, want := range []string{"result-card", "result-card-top", "source-pill src-signal", "result-snippet", "<mark>"} {
		if !contains(body, want) {
			t.Errorf("server-rendered results missing marker %q", want)
		}
	}
	// Results link into jump-to-context, and the attachment / link glyphs render.
	if !contains(body, `href="/c/`+itoa(lab)+`/at/`) {
		t.Error("result card lost its jump-to-context link")
	}
	if !contains(body, "result-glyph") {
		t.Error("result card lost its attachment / link glyph")
	}
	if cards := cardSequence(body); len(cards) != 4 {
		t.Errorf("rendered %d result cards, want 4", len(cards))
	}
}

// TestSearchFullPageMatchesPartialForEveryFilter is the acceptance criterion for
// issue #59: every filter, applied to the server-rendered page, must produce
// exactly the cards the HTMX path produces for the same query string — same
// matches, same ordering — and the form must re-render holding that state.
func TestSearchFullPageMatchesPartialForEveryFilter(t *testing.T) {
	srv, st, _ := newTestServer(t)
	lab := seedFilterLab(t, st)
	harper, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || harper == nil {
		t.Fatalf("fixture conversation Harper: %v", err)
	}

	cases := []struct {
		name  string
		query string
		want  int
		echo  []string
	}{
		{"query only", "q=fluvial", 4, []string{`name="q" value="fluvial"`}},
		{"conversation", "q=fluvial&conversation=" + itoa(lab), 4,
			[]string{fmt.Sprintf(`value="%s" selected`, itoa(lab))}},
		{"other conversation", "q=fluvial&conversation=" + itoa(harper.ID), 0, nil},
		{"source", "q=fluvial&source=signal", 4, []string{`value="signal" selected`}},
		{"unknown source ignored", "q=fluvial&source=carrierpigeon", 4, nil},
		{"sender", "q=fluvial&sender=Ada", 2, []string{`name="sender" value="Ada"`}},
		{"from", "q=fluvial&start=2022-04-01", 2, []string{`name="start" value="2022-04-01"`}},
		{"to", "q=fluvial&end=2022-03-31", 2, []string{`name="end" value="2022-03-31"`}},
		{"has attachment", "q=fluvial&has_attachment=1", 2, []string{`name="has_attachment" value="1" checked`}},
		{"has link", "q=fluvial&has_link=1", 2, []string{`name="has_link" value="1" checked`}},
		{"unparseable date is no bound", "q=fluvial&start=not-a-date", 4, []string{`name="start" value="not-a-date"`}},
		{"every filter at once", fmt.Sprintf(
			"q=fluvial&conversation=%s&source=signal&sender=Ada&start=2022-03-01&end=2022-03-31&has_attachment=1", itoa(lab)),
			1, []string{
				fmt.Sprintf(`value="%s" selected`, itoa(lab)),
				`value="signal" selected`,
				`name="sender" value="Ada"`,
				`name="start" value="2022-03-01"`,
				`name="end" value="2022-03-31"`,
				`name="has_attachment" value="1" checked`,
			}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, srv, "/search?"+tc.query)
			if page.Code != http.StatusOK {
				t.Fatalf("/search status = %d", page.Code)
			}
			frag := get(t, srv, "/search/results?"+tc.query)
			if frag.Code != http.StatusOK {
				t.Fatalf("/search/results status = %d", frag.Code)
			}
			pageBody, fragBody := page.Body.String(), frag.Body.String()

			full := cardSequence(pageBody)
			partial := cardSequence(fragBody)
			if len(full) != tc.want {
				t.Errorf("full page rendered %d cards (%v), want %d", len(full), full, tc.want)
			}
			if len(full) != len(partial) {
				t.Fatalf("no-JS path has %d cards, HTMX path has %d (%v vs %v)",
					len(full), len(partial), full, partial)
			}
			for i := range full {
				if full[i] != partial[i] {
					t.Errorf("card %d differs: full page %s, HTMX partial %s", i, full[i], partial[i])
				}
			}
			if tc.want == 0 && !contains(pageBody, "No messages match your search.") {
				t.Error("empty server-rendered result set lost its explanation")
			}
			for _, want := range tc.echo {
				if !contains(pageBody, want) {
					t.Errorf("form did not re-render with %q", want)
				}
			}
		})
	}
}

// TestSearchFullPageEmptyQueryShowsFormAndHint mirrors the partial's empty-query
// case on the page a JavaScript-less browser actually loads.
func TestSearchFullPageEmptyQueryShowsFormAndHint(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/search")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Type a query") {
		t.Errorf("empty query should show the hint, got:\n%.300q", body)
	}
	if !contains(body, `<form action="/search" method="get"`) {
		t.Error("empty query lost the form there is nothing without it")
	}
	if cards := cardSequence(body); len(cards) != 0 {
		t.Errorf("empty query rendered %d cards, want none", len(cards))
	}
}

// TestSearchFullPageFTSOperatorsNo500: the same FTS-operator hardening the
// partial endpoint pins must hold on the page a no-JS browser submits to.
func TestSearchFullPageFTSOperatorsNo500(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, q := range []string{"%22", "fluvial)", "NEAR(", "*", "fluvial+OR", "%22%22%22"} {
		if rec := get(t, srv, "/search?q="+q); rec.Code != http.StatusOK {
			t.Errorf("q=%q status = %d, want 200", q, rec.Code)
		}
	}
}

// TestSearchFullPageHighlightEscapesHTML is the XSS guarantee for the no-JS
// path: highlighting is applied by the same helper on both paths, and the full
// document must escape the body just as strictly as the fragment does.
func TestSearchFullPageHighlightEscapesHTML(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, "XSS Lab")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := time.Parse(signal.TimestampLayout, "2022-05-01 10:00:00")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, []signal.Message{
		{Conversation: "XSS Lab", Timestamp: parsed, TimestampRaw: "2022-05-01 10:00:00",
			Sender: "Mallory", Body: `<script>alert(1)</script> fluviorword`},
	}); err != nil {
		t.Fatal(err)
	}

	rec := get(t, srv, "/search?q=fluviorword")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if contains(body, "<script>alert(1)</script>") {
		t.Errorf("raw <script> leaked into the full search page (XSS): %s", body)
	}
	if !contains(body, "&lt;script&gt;") {
		t.Error("script tag was not HTML-escaped on the no-JS search page")
	}
	if !contains(body, "<mark>") {
		t.Error("highlight markup missing from the no-JS search page")
	}
}
