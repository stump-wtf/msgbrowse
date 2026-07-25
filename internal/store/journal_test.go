package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	_ "modernc.org/sqlite"
)

// TestMigrateV10ToV11AddsJournalTables walks the real migration chain to v10,
// then runs the runner and asserts schemaV11 lands the two journal tables plus
// the digest index, and that the FK-less migration commits cleanly.
func TestMigrateV10ToV11AddsJournalTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v10-to-v11.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout%285000%29")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	for v := 1; v <= 10; v++ {
		if _, err := db.ExecContext(ctx, migrations[v]); err != nil {
			t.Fatalf("apply v%d: %v", v, err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 10"); err != nil {
		t.Fatalf("stamp v10: %v", err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate v10→v11: %v", err)
	}
	if v, err := readUserVersion(ctx, db); err != nil || v != schemaVersion {
		t.Fatalf("user_version = %d (err %v), want %d", v, err, schemaVersion)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name IN
		 ('journal_days', 'journal_digests', 'idx_journal_digests_updated')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("journal schema objects present = %d, want 3", n)
	}
}

// TestBuildJournalDaysAggregates seeds a small cross-source, cross-day corpus
// and asserts the mechanical rollup: system and empty-body messages are
// excluded, days are newest-first, per-source counts and owner-excluded top
// senders are correct, and --since / exclude both narrow the result.
func TestBuildJournalDaysAggregates(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	harper, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, harper, source.Signal, []signal.Message{
		msg("Harper", "2023-05-01 09:00:00", "Harper", "morning", nil, nil),
		msg("Harper", "2023-05-01 09:05:00", signal.OwnerSender, "hi", nil, nil),
		msg("Harper", "2023-05-01 09:06:00", "Harper", "coffee?", nil, nil),
		sysMsg("Harper", "2023-05-01 09:10:00", "you called"),           // excluded: system
		msg("Harper", "2023-05-01 09:11:00", "Harper", "   ", nil, nil), // excluded: empty
		msg("Harper", "2023-05-02 12:00:00", "Harper", "next day", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
	mj, err := st.UpsertConversation(ctx, source.IMessage, "MJ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, mj, source.IMessage, []signal.Message{
		msg("MJ", "2023-05-01 20:00:00", "MJ", "yo", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}

	days, err := st.BuildJournalDays(ctx, "", nil)
	if err != nil {
		t.Fatalf("BuildJournalDays: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %d, want 2 (%+v)", len(days), days)
	}
	// Newest first.
	if days[0].Day != "2023-05-02" || days[1].Day != "2023-05-01" {
		t.Fatalf("day order = [%s, %s], want [2023-05-02, 2023-05-01]", days[0].Day, days[1].Day)
	}

	d2 := days[0]
	if d2.MessageCount != 1 || d2.ConversationCount != 1 || d2.SourceCounts["signal"] != 1 {
		t.Errorf("2023-05-02 rollup = %+v, want 1 msg / 1 conv / signal:1", d2)
	}

	d1 := days[1]
	if d1.MessageCount != 4 {
		t.Errorf("2023-05-01 message_count = %d, want 4 (system+empty excluded)", d1.MessageCount)
	}
	if d1.ConversationCount != 2 {
		t.Errorf("2023-05-01 conversation_count = %d, want 2", d1.ConversationCount)
	}
	if d1.SourceCounts["signal"] != 3 || d1.SourceCounts["imessage"] != 1 {
		t.Errorf("2023-05-01 source_counts = %v, want signal:3 imessage:1", d1.SourceCounts)
	}
	if len(d1.TopSenders) != 2 || d1.TopSenders[0] != (SenderCount{Name: "Harper", Count: 2}) {
		t.Errorf("2023-05-01 top_senders = %+v, want Harper:2 first", d1.TopSenders)
	}
	for _, s := range d1.TopSenders {
		if s.Name == signal.OwnerSender {
			t.Errorf("owner %q must not appear in top_senders", signal.OwnerSender)
		}
	}

	// --since floor.
	since, err := st.BuildJournalDays(ctx, "2023-05-02", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(since) != 1 || since[0].Day != "2023-05-02" {
		t.Errorf("since 2023-05-02 = %+v, want just that day", since)
	}

	// Exclude a conversation by name: its content vanishes from every count.
	excl, err := st.BuildJournalDays(ctx, "", []string{"MJ"})
	if err != nil {
		t.Fatal(err)
	}
	var got *JournalDay
	for i := range excl {
		if excl[i].Day == "2023-05-01" {
			got = &excl[i]
		}
	}
	if got == nil {
		t.Fatal("2023-05-01 missing after exclude")
	}
	if got.MessageCount != 3 || got.ConversationCount != 1 || got.SourceCounts["imessage"] != 0 {
		t.Errorf("2023-05-01 after exclude MJ = %+v, want 3 msg / 1 conv / no imessage", got)
	}
}

// TestBuildJournalDaysBucketsLegacyTimestampByUnix proves the mechanical layer
// buckets by ts_unix (date(ts_unix,'unixepoch')), not the ts string: a
// not-yet-re-ingested iMessage row still carries a legacy "Nov 13, 2015 …"
// timestamp string whose first 10 chars are not a valid day, but ts_unix is
// always canonical — the day must resolve from it, and DayTranscript must find
// the same key.
func TestBuildJournalDaysBucketsLegacyTimestampByUnix(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.IMessage, "MJ")
	if err != nil {
		t.Fatal(err)
	}
	const raw = "Nov 13, 2015 5:53:29 AM"
	parsed, perr := time.Parse("Jan 2, 2006 3:04:05 PM", raw)
	if perr != nil {
		t.Fatal(perr)
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.IMessage, []signal.Message{
		{Conversation: "MJ", Timestamp: parsed, TimestampRaw: raw, Sender: "MJ", Body: "legacy line"},
	}); err != nil {
		t.Fatal(err)
	}

	days, err := st.BuildJournalDays(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0].Day != "2015-11-13" {
		t.Fatalf("days = %+v, want a single day 2015-11-13 (bucketed by ts_unix, not the legacy string)", days)
	}
	lines, err := st.DayTranscript(ctx, "2015-11-13", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Body != "legacy line" {
		t.Fatalf("transcript = %+v, want the legacy line under 2015-11-13", lines)
	}
}

// TestJournalDayUTCBoundary proves both the mechanical grouping and the
// transcript window (ts_unix range) treat day boundaries as UTC and agree:
// messages 30 minutes either side of midnight land on their own days, with no
// localtime shift.
func TestJournalDayUTCBoundary(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	conv, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, conv, source.Signal, []signal.Message{
		msg("Harper", "2023-05-01 23:30:00", "Harper", "late", nil, nil),
		msg("Harper", "2023-05-02 00:30:00", "Harper", "early", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}

	days, err := st.BuildJournalDays(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %d, want 2 (boundary messages must split)", len(days))
	}
	for _, d := range days {
		if d.MessageCount != 1 {
			t.Errorf("day %s message_count = %d, want 1", d.Day, d.MessageCount)
		}
	}

	late, err := st.DayTranscript(ctx, "2023-05-01", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(late) != 1 || late[0].Body != "late" {
		t.Errorf("2023-05-01 transcript = %+v, want just the 23:30 'late' message", late)
	}
	early, err := st.DayTranscript(ctx, "2023-05-02", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(early) != 1 || early[0].Body != "early" {
		t.Errorf("2023-05-02 transcript = %+v, want just the 00:30 'early' message", early)
	}
}

// TestDigestCacheAndListJournalDays covers the digest cache round trip and the
// web listing's LEFT JOIN: a day lists with its digest when cached and falls
// back to the mechanical rollup after ResetDigests.
func TestDigestCacheAndListJournalDays(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	conv, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, conv, source.Signal, []signal.Message{
		msg("Harper", "2023-05-01 09:00:00", "Harper", "hello", nil, nil),
	}); err != nil {
		t.Fatal(err)
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

	// No digest yet.
	if _, _, _, ok, err := st.GetDayDigest(ctx, "2023-05-01"); err != nil || ok {
		t.Fatalf("GetDayDigest before put = ok %v err %v, want false,nil", ok, err)
	}

	if err := st.PutDayDigest(ctx, JournalDigest{
		Day: "2023-05-01", Model: "m1", PromptVersion: "pv1", Body: "A calm morning hello.",
	}); err != nil {
		t.Fatal(err)
	}
	body, model, pv, ok, err := st.GetDayDigest(ctx, "2023-05-01")
	if err != nil || !ok || body != "A calm morning hello." || model != "m1" || pv != "pv1" {
		t.Fatalf("GetDayDigest = (%q,%q,%q,%v,%v), want the stored row", body, model, pv, ok, err)
	}

	// Listing joins the digest onto the mechanical day.
	list, err := st.ListJournalDays(ctx, "", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Day != "2023-05-01" || list[0].DigestBody != "A calm morning hello." {
		t.Fatalf("ListJournalDays = %+v, want one day carrying the digest", list)
	}
	if list[0].MessageCount != 1 {
		t.Errorf("listed day message_count = %d, want 1", list[0].MessageCount)
	}

	// Reset clears digests but keeps the mechanical row.
	if err := st.ResetDigests(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, _ := st.GetDayDigest(ctx, "2023-05-01"); ok {
		t.Error("digest still present after ResetDigests")
	}
	list, err = st.ListJournalDays(ctx, "", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].DigestBody != "" {
		t.Errorf("after reset ListJournalDays = %+v, want mechanical day with empty digest", list)
	}
}

// TestDayTranscriptEnrichesAndExcludes checks the transcript carries per-line
// conversation identity, enriches attachments/links, drops system messages, and
// honors the exclude denylist so an excluded thread never appears.
func TestDayTranscriptEnrichesAndExcludes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	harper, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, harper, source.Signal, []signal.Message{
		msg("Harper", "2023-05-01 09:00:00", "Harper", "look at this",
			[]signal.Attachment{{Kind: signal.KindImage, RelPath: "media/a.jpg", OriginalName: "a.jpg"}},
			[]signal.Link{{URL: "https://example.com/x"}}),
		sysMsg("Harper", "2023-05-01 09:05:00", "you called"), // must not appear
	}); err != nil {
		t.Fatal(err)
	}
	mj, err := st.UpsertConversation(ctx, source.IMessage, "MJ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, mj, source.IMessage, []signal.Message{
		msg("MJ", "2023-05-01 20:00:00", "MJ", "secret thread", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}

	lines, err := st.DayTranscript(ctx, "2023-05-01", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("transcript lines = %d, want 2 (system excluded)", len(lines))
	}
	var harperLine *DayTranscriptLine
	for i := range lines {
		if lines[i].ConversationName == "Harper" {
			harperLine = &lines[i]
		}
	}
	if harperLine == nil {
		t.Fatal("Harper line missing")
	}
	if harperLine.Source != source.Signal {
		t.Errorf("Harper line source = %q, want %q", harperLine.Source, source.Signal)
	}
	if len(harperLine.Attachments) != 1 || len(harperLine.Links) != 1 {
		t.Errorf("Harper line children = %d attachments / %d links, want 1 / 1",
			len(harperLine.Attachments), len(harperLine.Links))
	}

	// Excluding MJ drops its line entirely.
	only, err := st.DayTranscript(ctx, "2023-05-01", []string{"MJ"})
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].ConversationName != "Harper" {
		t.Errorf("transcript after exclude MJ = %+v, want only Harper", only)
	}
}
