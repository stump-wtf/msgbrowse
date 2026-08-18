package web

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/source"
)

// getTemplateContent reads a template file from the embedded FS.
func getTemplateContent(t *testing.T, name string) string {
	t.Helper()
	b, err := templatesFS.ReadFile("templates/" + name)
	if err != nil {
		t.Fatalf("read template %s: %v", name, err)
	}
	return string(b)
}

// TestStatusBannerRendersAllVariants verifies the four variants render with
// the correct CSS class, ARIA role, and icon (SPEC-0013 §Accessibility —
// colour is never the sole signal, and failures are assertive).
//
// The contact merge endpoint drives three of the four variants (success,
// info, warning); the fourth (error) is verified at the template level in
// TestStatusBannerNoHandrolledMarkup.
func TestStatusBannerRendersAllVariants(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	// Create two contacts to merge (→ success).
	sig, err := st.UpsertConversation(ctx, source.Signal, "+15557770001")
	if err != nil {
		t.Fatal(err)
	}
	im, err := st.UpsertConversation(ctx, source.IMessage, "+15557770002")
	if err != nil {
		t.Fatal(err)
	}
	a := contactIDForConv(t, st, sig)
	b := contactIDForConv(t, st, im)

	// Create one contact for the "same" (→ info) variant.
	same, err := st.UpsertConversation(ctx, source.Signal, "+15557770003")
	if err != nil {
		t.Fatal(err)
	}
	sameID := contactIDForConv(t, st, same)

	tests := []struct {
		name      string
		fields    map[string]string
		wantClass string
		wantRole  string
		wantSVG   string
		wantTitle string
	}{
		{
			name:      "success-merge-ok",
			fields:    map[string]string{"a": strconv.FormatInt(a, 10), "b": strconv.FormatInt(b, 10)},
			wantClass: "notice-ok",
			wantRole:  `role="status"`,
			wantSVG:   `d="M9 12.75 11.25 15 15 9.75M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z"`,
			wantTitle: "Contacts merged.",
		},
		{
			name:      "info-merge-same",
			fields:    map[string]string{"a": strconv.FormatInt(sameID, 10), "b": strconv.FormatInt(sameID, 10)},
			wantClass: "notice-info",
			wantRole:  `role="status"`,
			wantSVG:   `d="M11.25 11.25l.041-.02`,
			wantTitle: "Nothing to merge.",
		},
		{
			name:      "warning-merge-invalid",
			fields:    map[string]string{"a": "bad", "b": "alsobad"},
			wantClass: "notice-warn",
			wantRole:  `role="status"`,
			wantSVG:   `d="M12 9v3.75m-9.303 3.376`,
			wantTitle: "That merge could not be read.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tok := mintToken(t, srv)
			rec := contactPOST(t, srv, "/settings/contacts/merge", selfOrigin, tok, tc.fields, nil)
			body := rec.Body.String()
			if !strings.Contains(body, tc.wantClass) {
				t.Errorf("response missing CSS class %q", tc.wantClass)
			}
			if !strings.Contains(body, tc.wantRole) {
				t.Errorf("response missing ARIA role %q", tc.wantRole)
			}
			if !strings.Contains(body, tc.wantSVG) {
				t.Errorf("response missing icon SVG path %q", tc.wantSVG)
			}
			if !strings.Contains(body, tc.wantTitle) {
				t.Errorf("response missing title %q", tc.wantTitle)
			}
		})
	}
}

// TestStatusBannerErrorIsAssertive verifies that the error variant uses
// role="alert" (assertive) in the partial's template source, while the
// success variant uses role="status" (polite).
func TestStatusBannerErrorIsAssertive(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// Success banner from rules POST uses role=status (polite).
	tok := mintToken(t, srv)
	rec := contactPOST(t, srv, "/settings/contacts/rules", selfOrigin, tok, map[string]string{"match_phone": "1"}, nil)
	body := rec.Body.String()

	if !strings.Contains(body, `role="status"`) {
		t.Error("success banner must use role=\"status\" (polite)")
	}
	if strings.Contains(body, `role="alert"`) {
		t.Error("success banner must NOT use role=\"alert\"")
	}

	// The partial template source must render role="alert" for the error
	// variant. Verify by inspecting the partial definition.
	partial := getTemplateContent(t, "partials.html")
	if !strings.Contains(partial, `$role = "alert"`) {
		t.Error("status_banner partial must set role=alert for error variant")
	}
}

// TestStatusBannerNoHandrolledMarkup verifies that no enum-driven banner
// call site uses bare notice-card without a variant class. The acceptance
// criteria states grep -c 'class="notice-card"' across templates/ returns
// only non-feedback uses.
func TestStatusBannerNoHandrolledMarkup(t *testing.T) {
	// These templates have enum-driven banners that MUST all use the
	// status_banner partial (with a notice-* variant class).
	templates := []string{
		"contact_settings.html",
		"llm_settings.html",
		"settings.html",
		"status.html",
		"backups.html",
	}

	for _, tmpl := range templates {
		t.Run(tmpl, func(t *testing.T) {
			body := getTemplateContent(t, tmpl)
			// The status_banner partial call must appear.
			if !strings.Contains(body, `{{template "status_banner"`) {
				t.Errorf("%s must use the status_banner partial", tmpl)
			}
			// No bare feedback notice-card divs should remain (those that
			// had role="status" or role="alert" — the enum-driven banners).
			if strings.Contains(body, `<div class="notice-card" role="status">`) {
				t.Errorf("%s still has a hand-rolled notice-card with role=status", tmpl)
			}
			if strings.Contains(body, `<div class="notice-card" role="alert">`) {
				t.Errorf("%s still has a hand-rolled notice-card with role=alert", tmpl)
			}
			// Also check for notice-card with variant classes that aren't
			// going through the partial (backups.html had these before).
			for _, v := range []string{"notice-ok", "notice-err", "notice-warn", "notice-info"} {
				if strings.Contains(body, `<div class="notice-card `+v) {
					t.Errorf("%s has hand-rolled %s — use status_banner", tmpl, v)
				}
			}
		})
	}
}

// TestStatusBannerIconPresentForEachVariant verifies each variant carries
// an icon so the state survives greyscale (SPEC-0013 §Accessibility —
// colour is never the sole signal).
func TestStatusBannerIconPresentForEachVariant(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	sig, err := st.UpsertConversation(ctx, source.Signal, "+15557770010")
	if err != nil {
		t.Fatal(err)
	}
	im, err := st.UpsertConversation(ctx, source.IMessage, "+15557770011")
	if err != nil {
		t.Fatal(err)
	}
	a := contactIDForConv(t, st, sig)
	b := contactIDForConv(t, st, im)

	// success variant
	tok := mintToken(t, srv)
	rec := contactPOST(t, srv, "/settings/contacts/merge", selfOrigin, tok, map[string]string{"a": strconv.FormatInt(a, 10), "b": strconv.FormatInt(b, 10)}, nil)
	body := rec.Body.String()
	if !strings.Contains(body, "status-banner-icon") {
		t.Error("success banner missing status-banner-icon span")
	}

	// info variant (same id)
	tok = mintToken(t, srv)
	rec = contactPOST(t, srv, "/settings/contacts/merge", selfOrigin, tok, map[string]string{"a": strconv.FormatInt(a, 10), "b": strconv.FormatInt(a, 10)}, nil)
	body = rec.Body.String()
	if !strings.Contains(body, "status-banner-icon") {
		t.Error("info banner missing status-banner-icon span")
	}

	// warning variant (invalid ids)
	tok = mintToken(t, srv)
	rec = contactPOST(t, srv, "/settings/contacts/merge", selfOrigin, tok, map[string]string{"a": "bad", "b": "alsobad"}, nil)
	body = rec.Body.String()
	if !strings.Contains(body, "status-banner-icon") {
		t.Error("warning banner missing status-banner-icon span")
	}
}

// TestStatusBannerEnumMappingDocumented verifies the enum-to-variant mapping
// is documented in the status_banner partial's comment block.
func TestStatusBannerEnumMappingDocumented(t *testing.T) {
	body := getTemplateContent(t, "partials.html")
	for _, enum := range []string{"MergeResult", "SplitResult", "PairResult", "UnpairResult", "SaveResult", "TestResult", "IndexResult"} {
		if !strings.Contains(body, enum) {
			t.Errorf("status_banner partial comment must document the %s enum", enum)
		}
	}
}

// TestStatusBannerVariantInHTMLOutput verifies that the rendered HTML
// carries the variant class on the notice-card div.
func TestStatusBannerVariantInHTMLOutput(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	sig, err := st.UpsertConversation(ctx, source.Signal, "+15557770020")
	if err != nil {
		t.Fatal(err)
	}
	im, err := st.UpsertConversation(ctx, source.IMessage, "+15557770021")
	if err != nil {
		t.Fatal(err)
	}
	a := contactIDForConv(t, st, sig)
	b := contactIDForConv(t, st, im)

	tok := mintToken(t, srv)
	rec := contactPOST(t, srv, "/settings/contacts/merge", selfOrigin, tok, map[string]string{"a": strconv.FormatInt(a, 10), "b": strconv.FormatInt(b, 10)}, nil)
	body := rec.Body.String()

	if !strings.Contains(body, `class="notice-card notice-ok`) {
		t.Error("rendered HTML must carry the notice-ok variant class on the notice-card div")
	}
}
