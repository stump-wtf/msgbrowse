package whatsapp

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
)

var update = flag.Bool("update", false, "rewrite golden files")

// parseFixture parses testdata/result.json with a fixed UTC zone so golden
// output is deterministic across machines.
func parseFixture(t *testing.T) ([]Conversation, []ParseError) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "result.json"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	convs, skips, err := ParseAll(f, ParseOptions{ArchiveRoot: "testdata", Location: time.UTC})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return convs, skips
}

// findConv returns the conversation with the given display name.
func findConv(t *testing.T, convs []Conversation, name string) Conversation {
	t.Helper()
	for _, c := range convs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("conversation %q not found in %v", name, convNames(convs))
	return Conversation{}
}

func convNames(convs []Conversation) []string {
	names := make([]string, len(convs))
	for i, c := range convs {
		names[i] = c.Name
	}
	return names
}

// TestParseFixtureGolden pins the full parsed output of the committed
// synthetic fixture. Run `go test ./internal/whatsapp -update` to regenerate
// after an intentional mapping change.
func TestParseFixtureGolden(t *testing.T) {
	convs, _ := parseFixture(t)
	got, err := json.MarshalIndent(convs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", "golden.json")
	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("parsed output does not match testdata/golden.json (run with -update after intentional changes)\ngot:\n%s", got)
	}
}

// TestCanonicalTimestamps asserts REQ-0009-004: every emitted TimestampRaw is
// already in signal.TimestampLayout, derived from the epoch field — zero
// non-canonical output, no reliance on any render-side fallback.
func TestCanonicalTimestamps(t *testing.T) {
	canonical := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`)
	convs, _ := parseFixture(t)
	total := 0
	for _, c := range convs {
		for _, m := range c.Messages {
			total++
			if !canonical.MatchString(m.TimestampRaw) {
				t.Errorf("%s: non-canonical TimestampRaw %q", c.Name, m.TimestampRaw)
			}
			if got := m.Timestamp.Format(signal.TimestampLayout); got != m.TimestampRaw {
				t.Errorf("%s: Timestamp %q does not round-trip TimestampRaw %q", c.Name, got, m.TimestampRaw)
			}
		}
	}
	if total == 0 {
		t.Fatal("fixture produced no messages")
	}

	// Spot-check the epoch → wall-clock mapping (1748768400 = 2025-06-01
	// 09:00:00 UTC) and that a float epoch truncates to whole seconds.
	ada := findConv(t, convs, "Ada Fixture")
	if got := ada.Messages[0].TimestampRaw; got != "2025-06-01 09:00:00" {
		t.Errorf("epoch 1748768400 → %q, want 2025-06-01 09:00:00", got)
	}
	var float bool
	for _, m := range ada.Messages {
		if m.Body == "float timestamps still canonicalize" {
			float = true
			if m.TimestampRaw != "2025-06-02 08:30:00" {
				t.Errorf("float epoch → %q, want 2025-06-02 08:30:00", m.TimestampRaw)
			}
		}
	}
	if !float {
		t.Error("float-timestamp message missing")
	}
}

// TestMalformedEntriesSkipped asserts the REQ-0009-003 scenario: entries with
// a missing timestamp (or otherwise malformed) are skip-logged while every
// other message in the chat imports.
func TestMalformedEntriesSkipped(t *testing.T) {
	convs, skips := parseFixture(t)

	if len(skips) != 4 {
		t.Fatalf("skips = %d (%v), want 4", len(skips), skips)
	}
	bySig := map[string]bool{}
	for i := range skips {
		bySig[skips[i].Chat+"#"+skips[i].MessageID] = true
		if skips[i].Error() == "" {
			t.Error("empty ParseError message")
		}
	}
	for _, want := range []string{
		"15550001111@s.whatsapp.net#7",  // timestamp: null
		"15550001111@s.whatsapp.net#99", // entry is not an object
		"15550002222-1600000000@g.us#7", // missing from_me
		"badchat@s.whatsapp.net#",       // chat value is not an object
	} {
		if !bySig[want] {
			t.Errorf("expected skip %q, got %v", want, bySig)
		}
	}

	// The rest of each chat still imports (chat-level skip drops only badchat).
	if len(convs) != 5 {
		t.Fatalf("conversations = %v, want 5", convNames(convs))
	}
	if n := len(findConv(t, convs, "Ada Fixture").Messages); n != 8 {
		t.Errorf("Ada Fixture messages = %d, want 8 (two entries skipped)", n)
	}
	if n := len(findConv(t, convs, "Fixture Crew").Messages); n != 6 {
		t.Errorf("Fixture Crew messages = %d, want 6 (one entry skipped)", n)
	}
	for _, m := range findConv(t, convs, "Ada Fixture").Messages {
		if strings.Contains(m.Body, "must be skipped") {
			t.Errorf("skipped entry leaked into output: %q", m.Body)
		}
	}
}

// TestReactions asserts REQ-0009-005: the exporter's {actor: emoji} object
// maps to signal.Reactions (owner alias "You" → "Me"), ordered
// deterministically, and reaction text never lands in a body.
func TestReactions(t *testing.T) {
	convs, _ := parseFixture(t)

	ada := findConv(t, convs, "Ada Fixture")
	var reacted *signal.Message
	for i := range ada.Messages {
		if strings.HasPrefix(ada.Messages[i].Body, "Yes! Landing at 9am") {
			reacted = &ada.Messages[i]
		}
	}
	if reacted == nil {
		t.Fatal("reacted message not found")
	}
	want := []signal.Reaction{
		{Emoji: "❤️", Actor: "Ada Fixture"},
		{Emoji: "👍", Actor: signal.OwnerSender},
	}
	if !reflect.DeepEqual(reacted.Reactions, want) {
		t.Errorf("reactions = %+v, want %+v", reacted.Reactions, want)
	}

	crew := findConv(t, convs, "Fixture Crew")
	var booked *signal.Message
	for i := range crew.Messages {
		if crew.Messages[i].Body == "Booked the table" {
			booked = &crew.Messages[i]
		}
	}
	if booked == nil {
		t.Fatal("group reacted message not found")
	}
	if len(booked.Reactions) != 1 || booked.Reactions[0].Emoji != "🎉" || booked.Reactions[0].Actor != signal.OwnerSender {
		t.Errorf("group reactions = %+v", booked.Reactions)
	}

	// Reaction emoji never appear in any body (they were only ever present in
	// the reactions object, and the mapping must keep it that way).
	for _, c := range convs {
		for _, m := range c.Messages {
			for _, r := range m.Reactions {
				if strings.Contains(m.Body, r.Emoji) {
					t.Errorf("%s: body %q contains reaction emoji %q", c.Name, m.Body, r.Emoji)
				}
			}
		}
	}
}

// TestQuotedReplyNeverMergesIntoBody: the exporter's reply/quoted_data fields
// reference the quoted parent; the quoted text must not be duplicated into
// the replying message's body.
func TestQuotedReplyNeverMergesIntoBody(t *testing.T) {
	convs, _ := parseFixture(t)
	ada := findConv(t, convs, "Ada Fixture")
	var reply *signal.Message
	for i := range ada.Messages {
		if strings.HasPrefix(ada.Messages[i].Body, "Can't wait") {
			reply = &ada.Messages[i]
		}
	}
	if reply == nil {
		t.Fatal("reply message not found")
	}
	if strings.Contains(reply.Body, "quoted text must never re-enter a body") {
		t.Errorf("quoted_data leaked into body: %q", reply.Body)
	}
	if len(reply.Links) != 1 || reply.Links[0].URL != "https://example.com/trip" {
		t.Errorf("links = %+v, want the trip URL", reply.Links)
	}
}

// TestBodiesArePlainText: the exporter's <br> newline substitution is undone
// and no HTML markup (vCard anchors included) survives into bodies.
func TestBodiesArePlainText(t *testing.T) {
	convs, _ := parseFixture(t)
	ada := findConv(t, convs, "Ada Fixture")

	var multiline bool
	for _, m := range ada.Messages {
		if m.Body == "Yes! Landing at 9am\nSee you at the cafe & bring the map" {
			multiline = true
		}
	}
	if !multiline {
		t.Error("<br> was not converted to a newline")
	}

	for _, c := range convs {
		for _, m := range c.Messages {
			if strings.Contains(m.Body, "<br>") || strings.Contains(m.Body, "<a href") {
				t.Errorf("%s: exporter markup leaked into body %q", c.Name, m.Body)
			}
		}
	}
}

// TestVCardMessage: contact-card messages keep their prose as plain text and
// surface each .vcf as a file attachment named after the contact.
func TestVCardMessage(t *testing.T) {
	convs, _ := parseFixture(t)
	ada := findConv(t, convs, "Ada Fixture")
	var vcard *signal.Message
	for i := range ada.Messages {
		if strings.Contains(ada.Messages[i].Body, "vCard file(s)") {
			vcard = &ada.Messages[i]
		}
	}
	if vcard == nil {
		t.Fatal("vCard message not found")
	}
	if want := "This media include the following vCard file(s):\nCasey Fixture | Rowan Fixture"; vcard.Body != want {
		t.Errorf("vCard body = %q, want %q", vcard.Body, want)
	}
	if len(vcard.Attachments) != 2 {
		t.Fatalf("vCard attachments = %+v, want 2", vcard.Attachments)
	}
	if a := vcard.Attachments[0]; a.Kind != signal.KindFile ||
		a.RelPath != "Message/Media/vCards/CaseyFixture.vcf" || a.OriginalName != "Casey Fixture" {
		t.Errorf("vCard attachment = %+v", a)
	}
	if vcard.IsSystem {
		t.Error("vCard message must not be a system event")
	}
}

// TestMissingMedia: media=true with the exporter's missing sentinel keeps an
// attachment entry (pathless, labeled) so the transcript chip fallback
// renders — and the sentinel prose never becomes body text.
func TestMissingMedia(t *testing.T) {
	convs, _ := parseFixture(t)
	ada := findConv(t, convs, "Ada Fixture")
	var missing *signal.Message
	for i := range ada.Messages {
		for _, a := range ada.Messages[i].Attachments {
			if a.OriginalName == "The media is missing" {
				missing = &ada.Messages[i]
			}
		}
	}
	if missing == nil {
		t.Fatal("missing-media message not found")
	}
	if missing.Body != "" {
		t.Errorf("missing-media body = %q, want empty (sentinel is exporter prose)", missing.Body)
	}
	if missing.IsSystem {
		t.Error("missing-media message must not be a system event")
	}
	if len(missing.Attachments) != 1 || missing.Attachments[0].RelPath != "" {
		t.Errorf("attachments = %+v, want one pathless entry", missing.Attachments)
	}
}

// TestMediaAttachments: media paths are stored root-relative, kind follows
// the mime type, and captions become the body.
func TestMediaAttachments(t *testing.T) {
	convs, _ := parseFixture(t)
	ada := findConv(t, convs, "Ada Fixture")

	var photo, voice *signal.Message
	for i := range ada.Messages {
		for _, a := range ada.Messages[i].Attachments {
			switch a.OriginalName {
			case "photo-fixture-001.jpg":
				photo = &ada.Messages[i]
			case "audio-fixture-002.opus":
				voice = &ada.Messages[i]
			}
		}
	}
	if photo == nil || voice == nil {
		t.Fatal("media messages not found")
	}
	if a := photo.Attachments[0]; a.Kind != signal.KindImage ||
		a.RelPath != "Message/Media/15550001111@s.whatsapp.net/photo-fixture-001.jpg" {
		t.Errorf("photo attachment = %+v", a)
	}
	if photo.Body != "the view from the trail" {
		t.Errorf("caption body = %q", photo.Body)
	}
	if a := voice.Attachments[0]; a.Kind != signal.KindFile {
		t.Errorf("voice note kind = %q, want file (chip)", a.Kind)
	}

	// Sticker (image/webp) renders as an image; absolute media_base falls
	// back to the exporter's root-relative data part when the export moved.
	abs := findConv(t, convs, "Abs Path Fixture")
	var pdf, sticker *signal.Message
	for i := range abs.Messages {
		for _, a := range abs.Messages[i].Attachments {
			switch a.OriginalName {
			case "doc-fixture-003.pdf":
				pdf = &abs.Messages[i]
			case "sticker-fixture-004.webp":
				sticker = &abs.Messages[i]
			}
		}
	}
	if pdf == nil || sticker == nil {
		t.Fatal("abs-path media messages not found")
	}
	if a := pdf.Attachments[0]; a.Kind != signal.KindFile ||
		a.RelPath != "Message/Media/15550006666@s.whatsapp.net/doc-fixture-003.pdf" {
		t.Errorf("pdf attachment = %+v (absolute media_base must not leak)", a)
	}
	if a := sticker.Attachments[0]; a.Kind != signal.KindImage {
		t.Errorf("sticker kind = %q, want image", a.Kind)
	}

	// No stored RelPath is ever absolute (the iMessage lesson).
	for _, c := range convs {
		for _, m := range c.Messages {
			for _, a := range m.Attachments {
				if filepath.IsAbs(a.RelPath) {
					t.Errorf("%s: absolute RelPath stored: %q", c.Name, a.RelPath)
				}
			}
		}
	}
}

// TestRelativizeMedia covers the three path shapes: relative stays relative,
// absolute-under-root relativizes, absolute-elsewhere falls back to the
// relative data part (or basename as a last resort).
func TestRelativizeMedia(t *testing.T) {
	cases := []struct {
		name, base, data, root, want string
	}{
		{"relative-no-base", "", "Message/Media/x.jpg", "/archive", "Message/Media/x.jpg"},
		{"relative-base", "out/", "Message/x.jpg", "/archive", "out/Message/x.jpg"},
		{"absolute-under-root", "/archive/wa/", "Message/x.jpg", "/archive/wa", "Message/x.jpg"},
		{"absolute-nested-under-root", "/archive/wa/", "Message/x.jpg", "/archive", "wa/Message/x.jpg"},
		{"absolute-elsewhere", "/somewhere/else/", "Message/x.jpg", "/archive", "Message/x.jpg"},
		{"absolute-data-last-resort", "", "/somewhere/else/x.jpg", "/archive", "x.jpg"},
		{"no-root-absolute", "/somewhere/", "Message/x.jpg", "", "Message/x.jpg"},
	}
	for _, tc := range cases {
		if got := relativizeMedia(tc.base, tc.data, tc.root); got != tc.want {
			t.Errorf("%s: relativizeMedia(%q, %q, %q) = %q, want %q",
				tc.name, tc.base, tc.data, tc.root, got, tc.want)
		}
	}
}

// TestSendersAndSystem: from_me → OwnerSender; group members keep their
// resolved name or number; meta rows (and content-less internal rows) become
// system events; 1:1 counterpart messages take the conversation name.
func TestSendersAndSystem(t *testing.T) {
	convs, _ := parseFixture(t)

	crew := findConv(t, convs, "Fixture Crew")
	if !crew.IsGroup {
		t.Error("Fixture Crew should be a group (@g.us)")
	}
	bySender := map[string]int{}
	var system []signal.Message
	for _, m := range crew.Messages {
		bySender[m.Sender]++
		if m.IsSystem {
			system = append(system, m)
		}
	}
	if bySender["Bo Fixture"] != 2 || bySender["15550003333"] != 1 || bySender[signal.OwnerSender] != 1 {
		t.Errorf("group senders = %v", bySender)
	}
	if len(system) != 2 {
		t.Fatalf("system events = %d, want 2 (group rename + unsupported internal)", len(system))
	}
	for _, m := range system {
		if m.Sender != signal.SystemSender {
			t.Errorf("system sender = %q, want %q", m.Sender, signal.SystemSender)
		}
	}
	if system[0].Body != "The group name changed to Fixture Crew" {
		t.Errorf("group rename body = %q", system[0].Body)
	}
	if system[1].Body != "" {
		t.Errorf("unsupported internal message body = %q, want empty", system[1].Body)
	}

	ada := findConv(t, convs, "Ada Fixture")
	if !slicesContainSender(ada.Messages, "Ada Fixture") || !slicesContainSender(ada.Messages, signal.OwnerSender) {
		t.Errorf("1:1 senders missing: %v", convSenders(ada.Messages))
	}
	if ada.IsGroup {
		t.Error("1:1 chat flagged as group")
	}
}

func slicesContainSender(msgs []signal.Message, sender string) bool {
	for _, m := range msgs {
		if m.Sender == sender {
			return true
		}
	}
	return false
}

func convSenders(msgs []signal.Message) map[string]int {
	out := map[string]int{}
	for _, m := range msgs {
		out[m.Sender]++
	}
	return out
}

// TestNameFallbackAndCollision: a null chat name falls back to the JID local
// part, and two chats sharing a display name stay distinct conversations.
func TestNameFallbackAndCollision(t *testing.T) {
	convs, _ := parseFixture(t)
	names := convNames(convs)
	want := map[string]bool{
		"15550004444":               true, // name:null → JID local part
		"Ada Fixture":               true,
		"Ada Fixture (15550005555)": true, // display-name collision keeps both chats
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for n := range want {
		if !got[n] {
			t.Errorf("missing conversation %q in %v", n, names)
		}
	}
}

// TestReparseIdempotence: parsing the same fixture twice yields byte-identical
// conversations and identical message content hashes.
func TestReparseIdempotence(t *testing.T) {
	first, firstSkips := parseFixture(t)
	second, secondSkips := parseFixture(t)
	if !reflect.DeepEqual(first, second) {
		t.Error("re-parse produced different conversations")
	}
	if len(firstSkips) != len(secondSkips) {
		t.Errorf("skip counts differ: %d vs %d", len(firstSkips), len(secondSkips))
	}
	for i := range first {
		for j := range first[i].Messages {
			a, b := first[i].Messages[j], second[i].Messages[j]
			if a.ID() != b.ID() {
				t.Errorf("message hash changed on re-parse: %s vs %s", a.ID(), b.ID())
			}
		}
	}
}

// TestChronologicalOrder: messages are ordered by epoch; equal timestamps
// fall back to numeric key order (original database order).
func TestChronologicalOrder(t *testing.T) {
	convs, _ := parseFixture(t)
	for _, c := range convs {
		for i := 1; i < len(c.Messages); i++ {
			if c.Messages[i].Timestamp.Before(c.Messages[i-1].Timestamp) {
				t.Errorf("%s: messages out of order at %d", c.Name, i)
			}
		}
	}
	// The tie pair (same epoch): key "2" (the from_me message) precedes "3".
	ada := findConv(t, convs, "Ada Fixture")
	if !strings.HasPrefix(ada.Messages[1].Body, "Yes! Landing") || !strings.HasPrefix(ada.Messages[2].Body, "Can't wait") {
		t.Errorf("tie-break order wrong: %q then %q", ada.Messages[1].Body, ada.Messages[2].Body)
	}
}

// TestMapMessageVideoMime verifies that a video/* MIME type maps to KindVideo
// (issue #4), not KindFile.
func TestMapMessageVideoMime(t *testing.T) {
	ts := 1600000000.0
	fromMe := true
	mime := "video/mp4"
	data := "Message/Media/x/video-clip.mp4"
	m := &message{
		FromMe:    &fromMe,
		Timestamp: &ts,
		Media:     true,
		Mime:      &mime,
		Data:      &data,
	}
	out := mapMessage("Test", false, "", "", time.UTC, m)
	if len(out.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want 1", out.Attachments)
	}
	if out.Attachments[0].Kind != signal.KindVideo {
		t.Errorf("kind = %q, want %q", out.Attachments[0].Kind, signal.KindVideo)
	}
}
