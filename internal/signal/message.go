// Package signal parses the plaintext Markdown corpus produced by signal-export.
//
// The archive's export/ tree contains one folder per conversation, each with a
// chat.md file and an optional media/ directory. This package turns chat.md into
// structured [Message] records (with [Attachment], [Link], and [Reaction]
// children) using the exact format documented in the project spec. Reactions are
// signal-export's trailer line "(- <Name>: <emoji>, … -)" appended to a message
// body; the parser diverts that trailer onto the message's Reactions so it never
// leaks into the body or becomes a standalone message. It performs no I/O policy
// of its own beyond reading the io.Reader it is given; callers own file access and
// must treat the archive as read-only.
package signal

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"time"
)

// TimestampLayout is the wall-clock layout of a chat.md bracketed timestamp.
// signal-export writes local time with no zone, so it is parsed as UTC purely to
// obtain a stable, monotonic ordering key; the original text is preserved in
// [Message.TimestampRaw].
const TimestampLayout = "2006-01-02 15:04:05"

// OwnerSender is the sender name signal-export uses for the archive owner.
const OwnerSender = "Me"

// SystemSender is the sentinel sender for system/timeline events (e.g. calls,
// profile changes, disappearing-message timers). Such messages are kept but
// flagged via [Message.IsSystem].
const SystemSender = "No-Sender"

// AttachmentKind classifies an attachment referenced from a message body.
type AttachmentKind string

const (
	// KindImage is an inline image, written as ![alt](media/<file>).
	KindImage AttachmentKind = "image"
	// KindVideo is a video file (mp4, mov, etc.), written as [name](media/<file>).
	KindVideo AttachmentKind = "video"
	// KindFile is any other attachment, written as [name](media/<file>).
	KindFile AttachmentKind = "file"
)

// Attachment is a media reference extracted from a message body. RelPath is the
// target exactly as written in the Markdown (typically "media/<file>"), to be
// resolved relative to the owning conversation folder by the caller.
type Attachment struct {
	Kind         AttachmentKind
	RelPath      string
	OriginalName string // alt text (images) or link text (files)
}

// Link is an http(s) URL extracted from a message body.
type Link struct {
	URL string
}

// Reaction is an emoji reaction (a Signal reaction or an iMessage tapback) that a
// participant applied to a message. Emoji is the rendered glyph (iMessage tapback
// words are mapped to a representative emoji; Signal emoji pass through as-is) and
// Actor is the reactor's display name ("" when the source does not name them).
type Reaction struct {
	Emoji string
	Actor string
}

// Message is a single parsed chat.md entry. The tuple (Conversation, Sender) is
// the identity for a participant; sender display names are not globally unique,
// so callers must not assume uniqueness across conversations.
type Message struct {
	// Conversation is the conversation folder name (the identity key).
	Conversation string
	// Timestamp is the parsed timestamp (UTC) used for ordering and range
	// queries. See [TimestampLayout].
	Timestamp time.Time
	// TimestampRaw is the original bracketed timestamp text.
	TimestampRaw string
	// Sender is the display name. [OwnerSender] marks the archive owner;
	// [SystemSender] marks system events (see IsSystem).
	Sender string
	// Body is the full message text with internal newlines preserved. It may be
	// empty for attachment-only or system messages.
	Body string
	// IsSystem reports whether this is a [SystemSender] timeline event.
	IsSystem bool
	// Seq disambiguates messages that are otherwise byte-identical within a
	// conversation (same timestamp, sender, and body). It starts at 0 and is the
	// final component of the content hash, making ingestion idempotent.
	Seq int
	// Attachments and Links are extracted from Body.
	Attachments []Attachment
	Links       []Link
	// Reactions are the emoji reactions/tapbacks applied to this message. They are
	// parsed off the wire (Signal's "(- Name: emoji -)" trailer, iMessage's
	// "Tapbacks:" block) and never become standalone messages.
	Reactions []Reaction
}

// ID returns the stable content hash that uniquely keys this message for
// idempotent ingestion. It is the SHA-256 of (conversation, raw timestamp,
// sender, body, seq) with NUL separators, hex-encoded. Re-parsing the same
// chat.md yields identical IDs, so upserts never duplicate rows.
func (m *Message) ID() string {
	return messageHash(m.Conversation, m.TimestampRaw, m.Sender, m.Body, m.Seq)
}

// HashWithSource returns the storage key for this message, namespaced by source.
// With multiple sources, two conversations that share a display name (e.g.
// signal:"MJ" and imessage:"MJ") could otherwise produce identical [Message.ID]
// values and collide on the globally-unique messages.hash. Folding the source
// into the conversation component keeps each source's messages distinct while
// remaining stable and idempotent. The store uses this as the messages.hash.
func (m *Message) HashWithSource(source string) string {
	return messageHash(source+"\x00"+m.Conversation, m.TimestampRaw, m.Sender, m.Body, m.Seq)
}

// messageHash computes the content hash described on [Message.ID].
func messageHash(conv, tsRaw, sender, body string, seq int) string {
	h := sha256.New()
	for _, part := range []string{conv, tsRaw, sender, body} {
		_, _ = io.WriteString(h, part)
		_, _ = h.Write([]byte{0})
	}
	var b [8]byte
	u := uint64(seq)
	for i := range b {
		b[i] = byte(u >> (8 * i))
	}
	_, _ = h.Write(b[:])
	return hex.EncodeToString(h.Sum(nil))
}
