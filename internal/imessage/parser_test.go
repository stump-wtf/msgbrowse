package imessage

import (
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/signal"
)

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		in       string
		wantRaw  string
		wantOK   bool
		wantYear int
	}{
		{"May 20, 2020  9:10:11 AM", "2020-05-20 09:10:11", true, 2020}, // space-padded single-digit hour
		{"May 20, 2020 10:00:00 AM", "2020-05-20 10:00:00", true, 2020}, // double-digit hour
		{"Jun 05, 2020  2:30:00 PM", "2020-06-05 14:30:00", true, 2020}, // zero-padded day + PM
		{"Hey, are we still on?", "", false, 0},                         // body line
		{"Me", "", false, 0},                                            // sender line
		{"", "", false, 0},
	}
	for _, tt := range tests {
		raw, ts, ok := parseTimestamp(tt.in)
		if ok != tt.wantOK {
			t.Errorf("parseTimestamp(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			continue
		}
		if ok {
			if raw != tt.wantRaw {
				t.Errorf("parseTimestamp(%q) raw = %q, want %q", tt.in, raw, tt.wantRaw)
			}
			if ts.Year() != tt.wantYear {
				t.Errorf("parseTimestamp(%q) year = %d, want %d", tt.in, ts.Year(), tt.wantYear)
			}
		}
	}
}

func TestParseMessages(t *testing.T) {
	const in = `May 20, 2020  9:10:11 AM
Me
Hey, are we still on for tomorrow?

May 20, 2020  9:12:00 AM
MJ
Yep, 10am works
this is a second line

May 20, 2020  9:15:00 AM
MJ
Here's the cabin photo
attachments/AB/CD/cabin.jpeg

May 20, 2020  9:16:00 AM
Me
Check this out https://maps.example.com/cabin
Tapbacks:
    Loved by MJ

May 20, 2020 10:00:00 AM
MJ
ok
This message responded to an earlier message.
`
	msgs, err := ParseAll("MJ", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5: %+v", len(msgs), msgs)
	}

	// m0: owner, single-line body.
	if msgs[0].Sender != "Me" || msgs[0].Body != "Hey, are we still on for tomorrow?" {
		t.Errorf("m0 = %+v", msgs[0])
	}
	// m1: multi-line body preserved.
	if msgs[1].Sender != "MJ" || msgs[1].Body != "Yep, 10am works\nthis is a second line" {
		t.Errorf("m1 body = %q", msgs[1].Body)
	}
	// m2: image attachment detected, path line not in body.
	if len(msgs[2].Attachments) != 1 || msgs[2].Attachments[0].Kind != signal.KindImage {
		t.Errorf("m2 attachments = %+v", msgs[2].Attachments)
	}
	if msgs[2].Attachments[0].RelPath != "attachments/AB/CD/cabin.jpeg" {
		t.Errorf("m2 attachment path = %q", msgs[2].Attachments[0].RelPath)
	}
	if strings.Contains(msgs[2].Body, "cabin.jpeg") {
		t.Errorf("m2 body should not contain the attachment path: %q", msgs[2].Body)
	}
	// m3: link extracted; tapbacks CAPTURED as a reaction (not in body, no extra
	// message). "Loved by MJ" → ❤️ reaction, actor MJ.
	if len(msgs[3].Links) != 1 || msgs[3].Links[0].URL != "https://maps.example.com/cabin" {
		t.Errorf("m3 links = %+v", msgs[3].Links)
	}
	if strings.Contains(msgs[3].Body, "Tapbacks") || strings.Contains(msgs[3].Body, "Loved by") {
		t.Errorf("m3 body leaked tapbacks: %q", msgs[3].Body)
	}
	if len(msgs[3].Reactions) != 1 || msgs[3].Reactions[0].Emoji != "❤️" || msgs[3].Reactions[0].Actor != "MJ" {
		t.Errorf("m3 reactions = %+v, want one ❤️ by MJ", msgs[3].Reactions)
	}
	// m4: reply notice skipped from body.
	if msgs[4].Body != "ok" {
		t.Errorf("m4 body = %q, want \"ok\"", msgs[4].Body)
	}
}

// TestParseTapbacksCaptured verifies that a "Tapbacks:" block attaches its
// reactions to the CURRENT message (mapping standard words to emoji and passing
// custom emoji through) and never spawns a standalone message.
func TestParseTapbacksCaptured(t *testing.T) {
	const in = `May 20, 2020  9:00:00 AM
Me
nice photo!
Tapbacks:
    Loved by Harper
    Laughed by MJ
    🎉 by Sam

May 20, 2020  9:01:00 AM
MJ
agreed
`
	msgs, err := ParseAll("MJ", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	// Two messages only — the tapback block did NOT become its own message.
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (tapbacks must not be standalone): %+v", len(msgs), msgs)
	}
	if msgs[0].Body != "nice photo!" {
		t.Errorf("m0 body = %q, want %q", msgs[0].Body, "nice photo!")
	}
	want := []signal.Reaction{
		{Emoji: "❤️", Actor: "Harper"}, // Loved
		{Emoji: "😂", Actor: "MJ"},      // Laughed
		{Emoji: "🎉", Actor: "Sam"},     // custom emoji passes through
	}
	if len(msgs[0].Reactions) != len(want) {
		t.Fatalf("m0 reactions = %+v, want %+v", msgs[0].Reactions, want)
	}
	for i, w := range want {
		if msgs[0].Reactions[i] != w {
			t.Errorf("m0 reaction[%d] = %+v, want %+v", i, msgs[0].Reactions[i], w)
		}
	}
	// The following message is unaffected.
	if msgs[1].Sender != "MJ" || msgs[1].Body != "agreed" || len(msgs[1].Reactions) != 0 {
		t.Errorf("m1 = %+v", msgs[1])
	}
}

// TestParseAttachmentVsURL guards the /code-review fixes: a bare URL or a
// one-word filename in the body must NOT be misclassified as an attachment.
func TestParseAttachmentVsURL(t *testing.T) {
	const in = `May 20, 2020  9:00:00 AM
MJ
https://example.com/photo.png

May 20, 2020  9:01:00 AM
MJ
readme.txt

May 20, 2020  9:02:00 AM
MJ
real attachment below
attachments/AB/CD/IMG_0001.HEIC
`
	msgs, err := ParseAll("MJ", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	// URL stays in body and is extracted as a link, not an attachment.
	if len(msgs[0].Attachments) != 0 {
		t.Errorf("URL misclassified as attachment: %+v", msgs[0].Attachments)
	}
	if len(msgs[0].Links) != 1 || msgs[0].Links[0].URL != "https://example.com/photo.png" {
		t.Errorf("URL not extracted as link: %+v", msgs[0].Links)
	}
	// One-word filename (no slash) stays in body.
	if len(msgs[1].Attachments) != 0 || msgs[1].Body != "readme.txt" {
		t.Errorf("one-word filename misclassified: atts=%+v body=%q", msgs[1].Attachments, msgs[1].Body)
	}
	// A real slash-bearing local path IS an attachment.
	if len(msgs[2].Attachments) != 1 || msgs[2].Attachments[0].RelPath != "attachments/AB/CD/IMG_0001.HEIC" {
		t.Errorf("real attachment not detected: %+v", msgs[2].Attachments)
	}
}

// TestParseBodyDateNotSplit guards against a body line that begins with a date
// being mistaken for a new message timestamp.
func TestParseBodyDateNotSplit(t *testing.T) {
	const in = `May 20, 2020  9:00:00 AM
MJ
Jan 5, 2021 10:30:00 AM was when we landed
it was a long flight
`
	msgs, err := ParseAll("MJ", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (body date must not split)", len(msgs))
	}
	if msgs[0].Body != "Jan 5, 2021 10:30:00 AM was when we landed\nit was a long flight" {
		t.Errorf("body mangled by false split: %q", msgs[0].Body)
	}
}

// TestParseReadReceiptSuffix confirms a timestamp line with a trailing
// parenthesized read receipt is still detected and parsed.
func TestParseReadReceiptSuffix(t *testing.T) {
	const in = `May 20, 2020  9:00:00 AM (Read by them after 2 minutes)
MJ
hi
`
	msgs, err := ParseAll("MJ", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Sender != "MJ" || msgs[0].Body != "hi" {
		t.Fatalf("read-receipt timestamp not parsed: %+v", msgs)
	}
}

// TestParseSenderlessDropped confirms two adjacent timestamp lines do not emit
// an empty junk message.
func TestParseSenderlessDropped(t *testing.T) {
	const in = `May 20, 2020  9:00:00 AM
May 20, 2020  9:01:00 AM
MJ
hi
`
	msgs, err := ParseAll("MJ", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (empty first block dropped): %+v", len(msgs), msgs)
	}
	if msgs[0].Sender != "MJ" || msgs[0].Body != "hi" {
		t.Errorf("surviving message wrong: %+v", msgs[0])
	}
}

func TestParseOwnerVsHandle(t *testing.T) {
	const in = `Jun 05, 2020  2:30:00 PM
+15551234567
hi from a phone handle

Jun 05, 2020  2:31:00 PM
Me
reply
`
	msgs, err := ParseAll("Group", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d, want 2", len(msgs))
	}
	if msgs[0].Sender != "+15551234567" {
		t.Errorf("handle sender = %q", msgs[0].Sender)
	}
	if msgs[1].Sender != signal.OwnerSender {
		t.Errorf("owner sender = %q, want %q", msgs[1].Sender, signal.OwnerSender)
	}
}

func TestParseSeqDistinctForIdenticalMessages(t *testing.T) {
	const in = `May 20, 2020  9:00:00 AM
MJ
ping

May 20, 2020  9:00:00 AM
MJ
ping
`
	msgs, err := ParseAll("MJ", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d, want 2", len(msgs))
	}
	if msgs[0].Seq == msgs[1].Seq {
		t.Errorf("byte-identical messages share Seq %d", msgs[0].Seq)
	}
	if msgs[0].HashWithSource("imessage") == msgs[1].HashWithSource("imessage") {
		t.Errorf("byte-identical messages share storage hash")
	}
}

// TestParseVideoAttachment verifies that a .mp4 attachment path is classified
// as KindVideo (issue #4), not KindFile.
func TestParseVideoAttachment(t *testing.T) {
	const in = `May 20, 2020  9:00:00 AM
MJ
check this out
attachments/AB/CD/clip.mp4
`
	msgs, err := ParseAll("MJ", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if len(msgs[0].Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1: %+v", len(msgs[0].Attachments), msgs[0].Attachments)
	}
	if msgs[0].Attachments[0].Kind != signal.KindVideo {
		t.Errorf("attachment kind = %q, want %q", msgs[0].Attachments[0].Kind, signal.KindVideo)
	}
	if msgs[0].Attachments[0].RelPath != "attachments/AB/CD/clip.mp4" {
		t.Errorf("attachment path = %q", msgs[0].Attachments[0].RelPath)
	}
}
