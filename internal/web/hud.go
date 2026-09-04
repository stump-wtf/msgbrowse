package web

// The HUD Component (issue #395)
//
// A hud is a row of label/value tiles: Home's archive-freshness strip
// (Conversations / Messages / Newest message) is the reference shape every
// stat tile in the app should use. Before this it was hardcoded to those
// three fields under the name "stat_strip" (settings.html only), so
// contact.html hand-rolled the same markup twice with different cells
// instead of reusing it — the same copy-paste drift ADR-0012 already called
// out for card padding.
//
// hudData is what the "hud" template define (partials.html) needs: an
// ordered cell list plus the spacing utility the caller wants on the outer
// frame (callers differ — Home/Status want mb-4, the contact profile's two
// strips want mb-3 and mb-8 — so margin stays the caller's choice rather
// than baked into the define). Values arrive pre-formatted (comma grouping
// already applied) so the template stays a pure renderer with no notion of
// which fields are numeric.
//
// @joestump-agent 08/22/2026 - Added with the hud component work (#395):
// replaces the settings.html-only "stat_strip" define and contact.html's two
// hand-rolled strips.

// Cell count is not free-form: .hud is a fixed three-column grid, so a
// builder emitting anything other than three cells wraps onto a second row or
// leaves dead columns inside the border. TestHUDBuildersMatchTheGrid pins the
// two together, reading the column count out of input.css.
//
// hudCell is one label/value tile inside a hud.
type hudCell struct {
	Label string
	Value string
	// Small switches the value to the compact style long strings need — a
	// timestamp, an hour label — instead of the default large tabular
	// numeral size.
	Small bool
}

// hudData is the "hud" define's dot.
type hudData struct {
	Cells []hudCell
	// Class is the caller's spacing utility for the outer frame (e.g. "mb-4").
	Class string
	// Cols is the grid column count the frame should use; 0 means the 3-column
	// default. Only 4 is defined beyond the default (hud-4, #450) — a builder
	// wanting another width widens input.css and TestHUDBuildersMatchTheGrid
	// together, per the cell-count rule.
	Cols int
}

// archiveHUD builds the 3-cell archive-freshness strip shared by Home and
// Status (#224): Conversations / Messages / Newest message. Both surfaces
// carry the same three fields, so this is the one place that shape is built.
func archiveHUD(conversationCount, totalMessages int, newestTS string, class string) hudData {
	newest := "—"
	if newestTS != "" {
		newest = newestTS
	}
	return hudData{
		Class: class,
		Cells: []hudCell{
			{Label: "Conversations", Value: commaInt(int64(conversationCount))},
			{Label: "Messages", Value: commaInt(int64(totalMessages))},
			{Label: "Newest message", Value: newest, Small: true},
		},
	}
}
