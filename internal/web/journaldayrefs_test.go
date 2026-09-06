package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// #371: journal People and Notable links are clickable ONLY when they resolve
// against archive facts recorded for that day — matching, never filtering
// (amended REQ-0016-016).

// TestNormalizeLinkKey covers the lookup-key normalization a model string is
// matched with.
func TestNormalizeLinkKey(t *testing.T) {
	cases := []struct{ a, b string }{
		{"HTTPS://EXAMPLE.com/x/", "https://example.com/x"},
		{"https://example.com:443/a?b=1#frag", "https://example.com/a?b=1"},
		{" http://example.com ", "http://example.com/"},
	}
	for _, c := range cases {
		if normalizeLinkKey(c.a) != normalizeLinkKey(c.b) {
			t.Errorf("normalize(%q) != normalize(%q)", c.a, c.b)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "data:text/html,x", "not a url ::"} {
		// No-scheme/host strings must match nothing, including themselves via
		// the empty key (unreachable in resolve, but never equal a real key).
		if k := normalizeLinkKey(bad); k != "" && normalizeLinkKey(k) == k {
			t.Errorf("%q produced a matchable key %q", bad, k)
		}
	}
}

func TestUniqueParticipant(t *testing.T) {
	ps := []store.JournalDayParticipant{
		{ContactID: 7, Name: "Harper"},
		{ContactID: 9, Name: "harper"}, // different contact, same display name → ambiguous
		{ContactID: 4, Name: "Jon Stump"},
	}
	if got := uniqueParticipant(" harper ", ps); got != 0 {
		t.Errorf("ambiguous name resolved to %d", got)
	}
	if got := uniqueParticipant("jon stump", ps); got != 4 {
		t.Errorf("unique match = %d, want 4", got)
	}
	if got := uniqueParticipant("Nobody", ps); got != 0 {
		t.Errorf("absent match = %d", got)
	}
	if got := uniqueParticipant("", ps); got != 0 {
		t.Errorf("empty match = %d", got)
	}
}

const journalRefsDigest = `{"summary":"links day","people":["%s","Ghost"],"themes":[],"mood":"calm",
 "highlights":[],"standout_media":[],
 "notable_links":["https://ex.com/trip","https://ex.com/TRIP/","#fragment-only","javascript:alert(1)","https://plausible.example/unobserved"]}`

func TestJournalDayCardResolvesLinksAndPeople(t *testing.T) {
	srv, st := newJournalServer(t)
	ctx := context.Background()
	day := "2023-05-01"

	// Two people thread + one owner-side message carrying two links observed
	// that day (one stored with odd case/trailing slash to exercise matching).
	convID, err := st.UpsertConversationIdentity(ctx, source.Signal, "+15551110001", contacts.SourceIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []signal.Message{}
	mk := func(rawTS, sender, body string) signal.Message {
		ts, _ := time.Parse(signal.TimestampLayout, rawTS)
		return signal.Message{Conversation: "+15551110001", Timestamp: ts, TimestampRaw: rawTS, Sender: sender, Body: body}
	}
	msgs = append(msgs,
		mk(day+" 09:00:00", "+15551110001", "solar deal https://ex.com/trip follow up"),
		mk(day+" 09:05:00", "+15551110001", "again https://EX.com/trip/ same one"),
	)
	if _, err := st.ReplaceConversationMessages(ctx, convID, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}
	// Link extraction is an importer pass, not part of the store write, so
	// record the two observed URLs directly (one stored odd-case + slash to
	// prove the model's string is only ever a normalized lookup key).
	for _, u := range []string{"https://ex.com/trip", "https://EX.com/trip/"} {
		res, err := st.DB().ExecContext(ctx,
			`INSERT INTO links(message_id, conversation_id, ts_unix, url, domain)
			 SELECT id, conversation_id, ts_unix, ?, 'ex.com'
			   FROM messages WHERE conversation_id = ? ORDER BY id LIMIT 1`, u, convID)
		if err != nil || n0(res) == 0 {
			t.Fatalf("seed link %q: %v", u, err)
		}
	}
	days, err := st.BuildJournalDays(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range days {
		if err := st.PutJournalDay(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	// The person resolves from what the store actually recorded — the
	// auto-created contact behind this conversation; the matched entry's href
	// is the STORED url, not the model's string.
	ps, perr := st.JournalDayParticipants(context.Background(), day)
	if perr != nil || len(ps) == 0 {
		t.Fatalf("participants = %d, %v", len(ps), perr)
	}
	structured := fmt.Sprintf(`{"summary":"links day","people":[%q,"Nobody"],"themes":[],"mood":"calm","highlights":[],"standout_media":[],"notable_links":["https://ex.com/trip","javascript:alert(1)","https://plausible.example/unobserved"]}`, ps[0].Name)
	putDigest(t, st, day, "links day", "calm", structured)

	var card *dayCard
	view, vok, verr := st.GetJournalDay(context.Background(), day)
	if verr != nil || !vok {
		t.Fatalf("GetJournalDay: %v %v", vok, verr)
	}
	card = buildDayCard(view, "")
	if err := srv.resolveDayCard(context.Background(), day, card); err != nil {
		t.Fatal(err)
	}
	var personRef JournalPersonRef
	for _, p := range card.DigestPeople {
		if p.ContactID != 0 {
			personRef = p
			break
		}
	}
	if personRef.Name == "" || personRef.ContactID == 0 {
		t.Fatalf("unique participant did not resolve: %+v", card.DigestPeople)
	}
	if len(card.NotableLinks) != 3 {
		t.Fatalf("notable links = %+v", card.NotableLinks)
	}
	matched := card.NotableLinks[0].Text == "https://ex.com/trip" && strings.HasPrefix(card.NotableLinks[0].URL, "https://EX.com/trip")
	inertJS := card.NotableLinks[1].Text == "javascript:alert(1)" && card.NotableLinks[1].URL == ""
	inertUnobserved := card.NotableLinks[2].Text == "https://plausible.example/unobserved" && card.NotableLinks[2].URL == ""
	if !matched || !inertJS || !inertUnobserved {
		t.Errorf("resolution wrong: %+v", card.NotableLinks)
	}

	body := get(t, srv, "/journal?day="+day).Body.String()

	// Unique participant links to their page.
	if !strings.Contains(body, `href="/contact/`) {
		t.Error("resolved person chip did not link to /contact/")
	}
	// Hostile scheme and unobserved URL render as inert text: their text is on
	// the page (escaped), but no anchor's href may carry either.
	if strings.Contains(body, `href="javascript`) {
		t.Error("hostile scheme rendered as an anchor")
	}
	if !strings.Contains(body, "javascript:alert(1)") {
		t.Error("hostile-scheme entry missing from card")
	}
	if strings.Contains(body, `href="https://plausible.example`) {
		t.Error("unobserved URL rendered as an anchor")
	}
	if !strings.Contains(body, "https://plausible.example/unobserved") {
		t.Error("unobserved URL missing from card")
	}
	// The observed link anchors to the STORED url (odd-case stored row).
	if !strings.Contains(body, `href="https://EX.com/trip/"`) ||
		!strings.Contains(body, `target="_blank"`) ||
		!strings.Contains(body, `rel="noopener noreferrer nofollow"`) {
		t.Error("matched link did not render as an anchored stored URL")
	}
}

func TestJournalDayCardUnchangedWithoutArchiveFacts(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Harper", []string{"2023-05-01"})
	putDigest(t, st, "2023-05-01", "prose only day", "",
		`{"summary":"links day","people":["Zed Nobody"],"themes":[],"mood":"","highlights":[],"standout_media":[],"notable_links":["https://ex.com/trip"]}`)

	rec := httptest.NewRecorder()
	srv.renderJournalPage(rec, httptest.NewRequest(http.MethodGet, "/journal?day=2023-05-01", nil), "")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(body, `href="https://ex.com/trip"`) {
		t.Error("link rendered as anchor despite zero archive facts")
	}
	if strings.Contains(body, `href="/contact/`) {
		t.Error("person linked despite no resolvable participant")
	}
}

func n0(r interface{ RowsAffected() (int64, error) }) int64 {
	n, _ := r.RowsAffected()
	return n
}

// Adversarial probe added in review of #371: the matching layer is the whole
// security boundary here, so both directions of it are pinned. A hostile
// scheme must key to "" (matching nothing, hence never anchoring), and two
// genuinely different URLs must never collide into one key — a collision
// would let an injected string borrow a legitimate link's href.
func TestNormalizeLinkKeyRejectsHostileSchemes(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD4=",
		"vbscript:msgbox(1)",
		"file:///etc/passwd",
		"//evil.com/x",
		"/relative/path",
		"not a url at all",
		"",
		"   ",
	} {
		if k := normalizeLinkKey(raw); k != "" {
			t.Errorf("normalizeLinkKey(%q) = %q, want \"\" (must match nothing)", raw, k)
		}
	}
}

func TestNormalizeLinkKeyEquivalence(t *testing.T) {
	same := [][2]string{
		{"https://Example.COM/a", "https://example.com/a"},
		{"https://example.com:443/a", "https://example.com/a"},
		{"http://example.com:80/a", "http://example.com/a"},
		{"https://example.com/a#frag", "https://example.com/a"},
		{"https://example.com/a/", "https://example.com/a"},
		{"  https://example.com/a  ", "https://example.com/a"},
	}
	for _, p := range same {
		if normalizeLinkKey(p[0]) != normalizeLinkKey(p[1]) {
			t.Errorf("%q and %q should share a key, got %q vs %q", p[0], p[1], normalizeLinkKey(p[0]), normalizeLinkKey(p[1]))
		}
	}
	// Path case is significant, and so is the query: a server may serve
	// different content at /A and /a.
	diff := [][2]string{
		{"https://example.com/a", "http://example.com/a"},
		{"https://example.com/a", "https://example.com/b"},
		{"https://example.com/a", "https://evil.com/a"},
		{"https://example.com/a?x=1", "https://example.com/a?x=2"},
		{"https://example.com/A", "https://example.com/a"},
	}
	for _, p := range diff {
		if normalizeLinkKey(p[0]) == normalizeLinkKey(p[1]) {
			t.Errorf("%q and %q must NOT share a key (%q)", p[0], p[1], normalizeLinkKey(p[0]))
		}
	}
}

// TestUniqueParticipantNormalisesNames (#438): an old digest's smooshed
// "ChelseaStump" must resolve to the contact "Chelsea Stump" without
// rebuilding the digest; genuine ambiguity (the normalised key matching two
// different contacts) stays unlinked.
func TestUniqueParticipantNormalisesNames(t *testing.T) {
	participants := []store.JournalDayParticipant{
		{Name: "Chelsea Stump", ContactID: 7},
	}
	if got := uniqueParticipant("ChelseaStump", participants); got != 7 {
		t.Errorf("smooshed name resolved to %d, want contact 7", got)
	}
	// Ambiguity: the normalised key matches two different contacts → unlinked.
	ambiguous := []store.JournalDayParticipant{
		{Name: "Chelsea Stump", ContactID: 7},
		{Name: "chelsea stump", ContactID: 8},
	}
	if got := uniqueParticipant("ChelseaStump", ambiguous); got != 0 {
		t.Errorf("ambiguous name resolved to %d, want 0", got)
	}
}
