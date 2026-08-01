// Package imessage parses the plain-text corpus produced by imessage-exporter
// (https://github.com/ReagentX/imessage-exporter) with `-f txt`. It targets the
// 4.2.0 output format and turns each per-conversation <ChatName>.txt file into
// the shared [signal.Message] records the rest of msgbrowse consumes, tagged at
// import time with source="imessage".
//
// The 4.2.0 txt format renders each message as:
//
//	May 20, 2020  9:10:11 AM            ← timestamp (space-padded hour), optional " (Read by …)"
//	Me                                 ← sender ("Me", a contact name, or a handle)
//	the message body                   ← zero or more body lines
//	attachments/AB/CD/IMG_1234.HEIC    ← attachment path lines (no "Attachment:" prefix)
//	Tapbacks:                          ← reactions, with indented detail lines
//	    Loved by Sample
//	This message responded to an earlier message.
//	                                   ← blank line separates messages
//
// Because attachments appear as bare path lines with no marker, they are
// detected heuristically (a spaceless path ending in a known media extension).
//
// A "Tapbacks:" line opens a reaction block whose indented detail lines read
// "<reaction> by <reactor>" (e.g. "    Loved by Sample"). These are CAPTURED as
// [signal.Reaction]s on the current message — the standard tapback words (Loved,
// Liked, Disliked, Laughed, Emphasized, Questioned) are mapped to a representative
// emoji and a custom emoji tapback is passed through as-is — so a tapback never
// becomes a standalone message. Quoted replies (indented) and status notices are
// still skipped. This is best-effort and should be validated against a real
// export; the format is version-sensitive.
package imessage

import (
	"bufio"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
)

// timestampLayout parses an imessage-exporter timestamp after runs of spaces are
// collapsed to one (the source format space-pads single-digit hours).
const timestampLayout = "Jan 2, 2006 3:04:05 PM"

// tsCore is the timestamp itself: "Mon DD, YYYY  H:MM:SS AM/PM" (space-padded
// hour). timestampLineRe requires the WHOLE line to be that timestamp, optionally
// followed by a parenthesized read receipt — so a body line that merely *begins*
// with a date ("Jan 5, 2021 10:30:00 AM was great") is not mistaken for a new
// message. Group 1 captures the core for parsing.
const tsCore = `[A-Z][a-z]{2} \d{1,2}, \d{4}\s+\d{1,2}:\d{2}:\d{2} [AP]M`

var timestampLineRe = regexp.MustCompile(`^(` + tsCore + `)(?:\s*\(.*\))?\s*$`)

var spaceRun = regexp.MustCompile(`\s+`)

// attachmentExtRe matches a path ending in a known media/document extension.
// isAttachmentPath adds the path-shape and URL guards.
var attachmentExtRe = regexp.MustCompile(`(?i)\.(jpe?g|png|gif|heic|heif|webp|bmp|tiff?|mov|mp4|m4v|3gp|avi|mkv|caf|m4a|amr|aac|wav|mp3|pdf|vcf|zip|docx?|xlsx?|pptx?|csv|txt|html?)$`)

// isAttachmentPath reports whether a (trimmed) content line is an attachment
// path. imessage-exporter writes attachments as bare filesystem paths (relative
// "attachments/…" in copy mode, or absolute), so we require a spaceless,
// slash-bearing, non-URL token ending in a known extension. This deliberately
// excludes bare URLs (which contain "://" and stay in the body so they become
// links) and one-word body lines like "readme.txt" (no slash).
func isAttachmentPath(trimmed string) bool {
	if strings.ContainsAny(trimmed, " \t") || strings.Contains(trimmed, "://") {
		return false
	}
	if !strings.Contains(trimmed, "/") {
		return false
	}
	return attachmentExtRe.MatchString(trimmed)
}

// imageExts are extensions classified as images (others become file attachments).
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".heic": true, ".heif": true, ".webp": true, ".bmp": true, ".tif": true, ".tiff": true,
}

// videoExts are extensions classified as videos (others become file attachments).
var videoExts = map[string]bool{
	".mp4": true, ".mov": true, ".avi": true, ".webm": true,
	".mkv": true, ".m4v": true, ".mpg": true, ".mpeg": true, ".3gp": true,
}

// noticeLines are status lines emitted by imessage-exporter that are not message
// content and are skipped. "Tapbacks:" is intentionally NOT here: it opens a
// reaction block (see tapbackHeader / parseTapback) whose details are captured.
var noticeLines = map[string]bool{
	"This message responded to an earlier message.":   true,
	"This message was deleted from the conversation!": true,
	"Attachment missing!":                             true,
}

// tapbackHeader is the line that opens an iMessage reaction block.
const tapbackHeader = "Tapbacks:"

// tapbackEmoji maps imessage-exporter's standard tapback words to a representative
// emoji. Words outside this set (newer custom emoji reactions) are passed through
// verbatim by parseTapback.
var tapbackEmoji = map[string]string{
	"Loved":      "❤️",
	"Liked":      "👍",
	"Disliked":   "👎",
	"Laughed":    "😂",
	"Emphasized": "‼️",
	"Questioned": "❓",
}

// tapbackDetailRe matches an indented tapback detail line, e.g.
// "    Loved by Sample" or "    👍 by Sample". Group 1 is the reaction token
// (a standard word or a custom emoji), group 2 the reactor name.
var tapbackDetailRe = regexp.MustCompile(`^\s+(.+?) by (.+?)\s*$`)

// parseTapback turns an indented tapback detail line into a [signal.Reaction],
// mapping standard tapback words to emoji and passing custom emoji through. It
// returns ok=false for a line that does not match the "<reaction> by <reactor>"
// shape (the caller leaves the block and re-classifies the line).
func parseTapback(line string) (signal.Reaction, bool) {
	m := tapbackDetailRe.FindStringSubmatch(line)
	if m == nil {
		return signal.Reaction{}, false
	}
	token, actor := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
	emoji := token
	if mapped, ok := tapbackEmoji[token]; ok {
		emoji = mapped
	}
	return signal.Reaction{Emoji: emoji, Actor: actor}, true
}

// Parse streams an imessage-exporter txt file from r, emitting one
// [signal.Message] per message block for the given conversation. emit is called
// in file order; if it returns an error, Parse stops and returns it. onSkip, if
// non-nil, is called for non-blank content that appears before the first
// timestamp (a malformed/unsupported preamble).
func Parse(conversation string, r io.Reader, emit func(signal.Message) error, onSkip func(line int, text string)) error {
	br := bufio.NewReader(r)
	seq := newSeqCounter()

	var (
		cur        *signal.Message
		bodyLines  []string
		atts       []signal.Attachment
		reactions  []signal.Reaction
		inTapbacks bool
		expect     senderState
		lineNo     int
	)

	flush := func() error {
		if cur == nil {
			return nil
		}
		cur.Body = strings.TrimRight(strings.Join(bodyLines, "\n"), "\n")
		cur.Attachments = atts
		cur.Links = signal.ExtractLinks(cur.Body)
		cur.Reactions = reactions
		empty := cur.Sender == "" && cur.Body == "" && len(atts) == 0 && len(cur.Links) == 0 && len(reactions) == 0
		cur.Seq = seq.next(cur.Conversation, cur.TimestampRaw, cur.Sender, cur.Body)
		m := *cur
		cur, bodyLines, atts, reactions, inTapbacks, expect = nil, nil, nil, nil, false, stateNone
		if empty {
			// Drop junk: a timestamp with no sender, body, attachment, link, or
			// reaction (e.g. two adjacent timestamp lines). Never persist an empty row.
			return nil
		}
		return emit(m)
	}

	for {
		line, readErr := br.ReadString('\n')
		if len(line) > 0 || readErr == nil {
			lineNo++
			text := strings.TrimRight(line, "\r\n")

			if raw, ts, ok := parseTimestamp(text); ok {
				if err := flush(); err != nil {
					return err
				}
				cur = &signal.Message{Conversation: conversation, Timestamp: ts, TimestampRaw: raw}
				expect = stateSender
			} else if cur == nil {
				if strings.TrimSpace(text) != "" && onSkip != nil {
					onSkip(lineNo, text)
				}
			} else if expect == stateSender {
				cur.Sender = strings.TrimSpace(text)
				expect = stateBody
			} else if strings.TrimSpace(text) == tapbackHeader {
				// Open a tapback block: subsequent indented "<reaction> by <reactor>"
				// lines attach to THIS message rather than spawning a new one.
				inTapbacks = true
			} else if inTapbacks {
				if r, ok := parseTapback(text); ok {
					reactions = append(reactions, r)
				} else {
					// Not a tapback detail line — the block has ended; re-classify.
					inTapbacks = false
					classifyContent(text, &bodyLines, &atts)
				}
			} else {
				classifyContent(text, &bodyLines, &atts)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}
	return flush()
}

// ParseAll collects every message into a slice (for tests / small files).
func ParseAll(conversation string, r io.Reader) ([]signal.Message, error) {
	var msgs []signal.Message
	err := Parse(conversation, r, func(m signal.Message) error { msgs = append(msgs, m); return nil }, nil)
	return msgs, err
}

// classifyContent sorts a content line into a body line, an attachment, or
// nothing (skipped: blanks, indented tapback/reply detail, status notices).
func classifyContent(line string, bodyLines *[]string, atts *[]signal.Attachment) {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		// Blank line: message separator / spacing — not body content.
	case line != trimmed && strings.HasPrefix(line, " "):
		// Indented: tapback detail or a quoted reply (the quoted parent is its
		// own top-level block elsewhere) — skip.
	case noticeLines[trimmed]:
		// Status notice — not content.
	case isAttachmentPath(trimmed):
		kind := signal.KindFile
		ext := strings.ToLower(filepath.Ext(trimmed))
		if imageExts[ext] {
			kind = signal.KindImage
		} else if videoExts[ext] {
			kind = signal.KindVideo
		}
		*atts = append(*atts, signal.Attachment{
			Kind: kind, RelPath: trimmed, OriginalName: filepath.Base(trimmed),
		})
	default:
		*bodyLines = append(*bodyLines, line)
	}
}

// parseTimestamp returns the canonical timestamp text and parsed time if line
// begins with an imessage-exporter timestamp. The raw text is re-emitted in
// [signal.TimestampLayout] ("YYYY-MM-DD HH:MM:SS") — NOT the source-formatted
// string — so stored ts values sort chronologically as strings and render
// helpers see one canonical shape regardless of source. This changes the
// content hash of every iMessage message, so switching layouts requires a full
// re-ingest (reactions re-key automatically via ReplaceConversationMessages).
func parseTimestamp(line string) (raw string, t time.Time, ok bool) {
	m := timestampLineRe.FindStringSubmatch(line)
	if m == nil {
		return "", time.Time{}, false
	}
	norm := spaceRun.ReplaceAllString(strings.TrimSpace(m[1]), " ")
	parsed, err := time.Parse(timestampLayout, norm)
	if err != nil {
		return "", time.Time{}, false
	}
	return parsed.Format(signal.TimestampLayout), parsed, true
}

type senderState int

const (
	stateNone senderState = iota
	stateSender
	stateBody
)

// seqCounter assigns the per-conversation Seq disambiguator for byte-identical
// messages (mirrors the signal parser's counter).
type seqCounter struct{ counts map[string]int }

func newSeqCounter() *seqCounter { return &seqCounter{counts: map[string]int{}} }

func (s *seqCounter) next(conv, tsRaw, sender, body string) int {
	key := conv + "\x00" + tsRaw + "\x00" + sender + "\x00" + body
	n := s.counts[key]
	s.counts[key] = n + 1
	return n
}
