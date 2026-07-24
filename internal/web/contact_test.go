package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// newContactServer wires a Server over a fresh store seeded with one contact
// (a Signal thread) plus two AI facts — one benign, one containing markup for
// the escaping test. Returns the server, store, and the contact id.
func newContactServer(t *testing.T) (*Server, *store.Store, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "contact-web.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	mk := func(conv, ts, sender, body string) signal.Message {
		parsed, _ := time.Parse(signal.TimestampLayout, ts)
		return signal.Message{Conversation: conv, Timestamp: parsed, TimestampRaw: ts, Sender: sender, Body: body}
	}
	sig, err := st.UpsertConversation(ctx, source.Signal, "Chelsea")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, sig, source.Signal, []signal.Message{
		mk("Chelsea", "2022-10-22 04:17:13", "Chelsea", "look at this"),
		mk("Chelsea", "2022-10-22 04:18:02", signal.OwnerSender, "lol ordering one"),
	}); err != nil {
		t.Fatal(err)
	}
	var cid int64
	if err := st.DB().QueryRowContext(ctx, `SELECT contact_id FROM conversations WHERE id = ?`, sig).Scan(&cid); err != nil {
		t.Fatal(err)
	}

	var hash, ts string
	var tsUnix int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT hash, ts, ts_unix FROM messages WHERE conversation_id = ? ORDER BY ts_unix LIMIT 1`, sig).
		Scan(&hash, &ts, &tsUnix); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ fact, cat string }{
		{"Has a brother named Sean", "personal"},
		{"<script>alert(1)</script> likes hiking", "preferences"},
	} {
		if _, err := st.PutFact(ctx, store.FactInput{
			ContactID: cid, Fact: f.fact, Category: f.cat, Source: source.Signal,
			SourceMessageHash: hash, SourceTS: ts, SourceTSUnix: tsUnix, Model: "m",
		}); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{DataDir: t.TempDir()}
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, st, cid
}

func TestContactFullPageRenders(t *testing.T) {
	srv, st, cid := newContactServer(t)
	var sigConv int64
	_ = st.DB().QueryRow(`SELECT id FROM conversations WHERE contact_id = ? LIMIT 1`, cid).Scan(&sigConv)

	rec := get(t, srv, "/contact/"+itoa(cid))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<main id="main-content"`, "Chelsea", "Message volume", "AI-gathered facts",
		"Has a brother named Sean", "Messages", "Most active hour",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("full page missing %q", want)
		}
	}
	// Facts deep-link to the message's OWN conversation.
	if !strings.Contains(body, "/c/"+itoa(sigConv)+"/at/") {
		t.Errorf("fact deep-link should target /c/%d/at/…", sigConv)
	}
}

func TestContactPartialOmitsShell(t *testing.T) {
	srv, _, cid := newContactServer(t)
	req := httptest.NewRequest(http.MethodGet, "/contact/"+itoa(cid), nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") || strings.Contains(body, "<html") {
		t.Error("boosted partial must not carry the document shell")
	}
	if !strings.Contains(body, `<main id="main-content"`) {
		t.Error("partial should carry #main-content")
	}
}

// TestContactNoConversationsRenders guards the reviewed crash: a contact that
// exists but owns zero conversations (e.g. a cross-source identifier survives a
// source deletion) must render a page, not 500 on an empty-slice template index.
func TestContactNoConversationsRenders(t *testing.T) {
	srv, st, _ := newContactServer(t)
	ctx := context.Background()
	res, err := st.DB().ExecContext(ctx, `INSERT INTO contacts(display_name) VALUES ('Ghost')`)
	if err != nil {
		t.Fatal(err)
	}
	ghost, _ := res.LastInsertId()
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO contact_identifiers(contact_id, source, identifier) VALUES (?, 'imessage', '+15550000000')`, ghost); err != nil {
		t.Fatal(err)
	}
	rec := get(t, srv, "/contact/"+itoa(ghost))
	if rec.Code != http.StatusOK {
		t.Fatalf("zero-conversation contact status = %d, want 200 (must not crash)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Ghost") {
		t.Error("zero-conversation contact should still render its name")
	}
}

func TestContactNotFound(t *testing.T) {
	srv, _, _ := newContactServer(t)
	if rec := get(t, srv, "/contact/abc"); rec.Code != http.StatusNotFound {
		t.Errorf("bad id status = %d, want 404", rec.Code)
	}
	if rec := get(t, srv, "/contact/999999"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown contact status = %d, want 404", rec.Code)
	}
}

func TestContactEscapesFactMarkup(t *testing.T) {
	srv, _, cid := newContactServer(t)
	body := get(t, srv, "/contact/"+itoa(cid)).Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("fact markup rendered live — html/template escaping bypassed")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("fact markup should appear HTML-escaped")
	}
}
