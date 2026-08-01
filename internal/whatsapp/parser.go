// Package whatsapp parses the JSON corpus produced by WhatsApp-Chat-Exporter
// (https://github.com/KnugiHK/WhatsApp-Chat-Exporter) into the shared
// [signal.Message] records the rest of msgbrowse consumes, tagged at import
// time with source="whatsapp" (SPEC-0009, ADR-0016).
//
// The export is a single result.json: a top-level object keyed by chat JID
// ("<user>@s.whatsapp.net" for 1:1, "<id>@g.us" for groups, "<id>@lid" for
// linked-device identities). Each chat carries a display name, a media_base
// prefix, and a "messages" object keyed by database row id. The upstream
// schema is unversioned; the exact field semantics are pinned against the
// exporter's data_model.py / ios_handler.py / android_handler.py in
// docs/openspec/specs/whatsapp-source/design.md and defended by the committed
// fixtures under testdata/. Unknown JSON fields are ignored; malformed chats
// and messages are skip-logged via [ParseError] and never abort the parse
// (REQ-0009-003).
//
// Key mapping decisions (see design.md for the full field table):
//
//   - "timestamp" is the epoch-seconds field (data_model.Message.__init__
//     normalizes milliseconds away); TimestampRaw is formatted from it in
//     [signal.TimestampLayout] at parse time (REQ-0009-004). The exporter's
//     pre-formatted "time" / "received_timestamp" / "read_timestamp" strings
//     are ignored.
//   - "from_me" == true maps the sender to [signal.OwnerSender].
//   - "meta" == true without media marks a system/timeline event
//     ([signal.SystemSender], IsSystem). A data-less, media-less entry (the
//     exporter renders these as "Not supported WhatsApp internal message")
//     is treated the same way.
//   - "reactions" is an object of {actor: emoji}; it becomes
//     [signal.Reaction]s and never body text (REQ-0009-005). The exporter's
//     own-reaction actor "You" is mapped to [signal.OwnerSender].
//   - media messages carry their path in "data" (full path = media_base +
//     data, per the exporter's HTML template); paths are stored as
//     archive-root-relative RelPaths only — never absolute (the iMessage
//     absolute-path lesson). The missing-media sentinel data
//     ("The media is missing") is preserved as a pathless attachment so the
//     transcript's chip fallback still renders it.
//   - the exporter substitutes "<br>" for newlines and wraps vCard summaries
//     in HTML anchors; both are undone here so bodies are always plain text.
package whatsapp

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
)

// ResultFile is the exporter's JSON output filename inside the archive root.
// The CLI export wrapper directs `wtsexporter --json` at this name and doctor
// checks for its presence (SPEC-0009 REQ-0009-002/009).
const ResultFile = "result.json"

// groupSuffix marks a group chat JID. 1:1 chats end in "@s.whatsapp.net" (or
// "@lid" for linked-device identities); the chat "type" field is the device
// type ("ios"/"android"), NOT the chat kind.
const groupSuffix = "@g.us"

// mediaMissingSentinel is the exact "data" value the exporter writes when a
// message's media file was not exported (e.g. never synced to the backed-up
// device). Such messages also carry mime="media" and meta=true.
const mediaMissingSentinel = "The media is missing"

// vcardMime is the mime the exporter stamps on contact-card messages, whose
// "data" is an HTML summary with <a href="….vcf"> anchors.
const vcardMime = "text/x-vcard"

// ownReactionActor is the actor name the exporter uses for the archive
// owner's own reactions; it is mapped to signal.OwnerSender.
const ownReactionActor = "You"

// brRe matches the exporter's newline substitution: iOS writes "<br>",
// Android writes " <br>" (data_model handlers replace "\n"/"\r\n"). The
// optional leading space is consumed so round-tripping restores "\n".
var brRe = regexp.MustCompile(` ?<br>`)

// anchorRe matches the HTML anchors the exporter embeds in vCard summaries:
// <a href="path">label</a>. Group 1 is the href, group 2 the label.
var anchorRe = regexp.MustCompile(`<a href="([^"]*)">([^<]*)</a>`)

// tagRe strips residual HTML tags in exporter-generated summaries (vCard
// bodies only — user text is never tag-stripped, to avoid corrupting
// inequality-bearing messages like "1 < 2 > 0").
var tagRe = regexp.MustCompile(`<[^>]*>`)

// ParseError describes a malformed chat or message entry that the parser
// skipped. Ingestion logs these and continues; a bad entry never aborts the
// rest of the conversation (REQ-0009-003).
type ParseError struct {
	Chat      string // chat JID
	MessageID string // message key within the chat ("" for chat-level errors)
	Err       error
}

func (e *ParseError) Error() string {
	if e.MessageID == "" {
		return fmt.Sprintf("chat %s: %v", e.Chat, e.Err)
	}
	return fmt.Sprintf("chat %s message %s: %v", e.Chat, e.MessageID, e.Err)
}

// ParseOptions configures a parse.
type ParseOptions struct {
	// ArchiveRoot is the WhatsApp-Chat-Exporter output directory. It is used
	// only to relativize absolute media paths (media_base can be absolute
	// when the exporter was invoked with an absolute output dir); no I/O is
	// performed against it.
	ArchiveRoot string
	// Location is the zone used to render epoch timestamps as wall-clock
	// TimestampRaw strings, mirroring the local-time convention of the other
	// two sources. Defaults to time.Local.
	Location *time.Location
}

// Conversation is one parsed chat: its resolved display name, whether it is a
// group, and its messages in chronological order.
type Conversation struct {
	// JID is the chat's raw key in result.json (e.g. "1555…@s.whatsapp.net").
	JID string
	// Name is the display name: the chat's "name" field, falling back to the
	// JID's local part (the phone number for 1:1 chats). When two chats share
	// a display name the JID local part is appended in parentheses so each
	// JID keeps its own conversation (names key conversations in the store).
	Name string
	// IsGroup reports whether the JID is a group ("…@g.us").
	IsGroup bool
	// Messages are the chat's messages, ordered by timestamp (ties broken by
	// numeric message key, i.e. original database order).
	Messages []signal.Message
}

// chat mirrors the ChatStore fields we consume; unknown fields are ignored.
// Messages stay raw so one malformed message skips individually.
type chat struct {
	Name      *string                    `json:"name"`
	MediaBase string                     `json:"media_base"`
	Messages  map[string]json.RawMessage `json:"messages"`
}

// message mirrors the exporter's Message.to_json() fields we consume
// (data_model.py). Pointer fields distinguish null/absent from zero values.
// The pre-formatted "time", "received_timestamp", and "read_timestamp"
// strings plus "key_id", "safe", "thumb", and "message_type" are
// intentionally not mapped (see design.md).
type message struct {
	FromMe     *bool             `json:"from_me"`
	Timestamp  *float64          `json:"timestamp"` // epoch seconds (exporter normalizes ms)
	Data       *string           `json:"data"`
	Sender     *string           `json:"sender"`
	Meta       bool              `json:"meta"`
	Media      bool              `json:"media"`
	Mime       *string           `json:"mime"`
	Caption    *string           `json:"caption"`
	Sticker    bool              `json:"sticker"`
	Reply      *string           `json:"reply"`       // quoted parent key_id — not mapped to body
	QuotedData *string           `json:"quoted_data"` // quoted parent text — never merged into body
	Reactions  map[string]string `json:"reactions"`   // {actor: emoji}
}

// Parse decodes a result.json stream and emits one [Conversation] per chat,
// in deterministic (sorted-JID) order. emit is called once per chat; if it
// returns an error, Parse stops and returns it. onSkip, if non-nil, receives
// a [ParseError] for every malformed chat or message entry; parsing continues
// regardless, so a single bad entry never aborts the rest (REQ-0009-003).
func Parse(r io.Reader, opts ParseOptions, emit func(Conversation) error, onSkip func(ParseError)) error {
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}
	skip := func(e ParseError) {
		if onSkip != nil {
			onSkip(e)
		}
	}

	var top map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&top); err != nil {
		return fmt.Errorf("decode result.json: %w", err)
	}

	jids := make([]string, 0, len(top))
	for jid := range top {
		jids = append(jids, jid)
	}
	sort.Strings(jids)

	// Resolve display names first so collisions (two chats sharing a name)
	// can be disambiguated deterministically: the first (by sorted JID) keeps
	// the plain name, later ones get their JID local part appended.
	names := make(map[string]string, len(jids))
	taken := make(map[string]bool, len(jids))
	chats := make(map[string]*chat, len(jids))
	for _, jid := range jids {
		var c chat
		if err := json.Unmarshal(top[jid], &c); err != nil {
			skip(ParseError{Chat: jid, Err: fmt.Errorf("malformed chat: %w", err)})
			continue
		}
		chats[jid] = &c
		name := ""
		if c.Name != nil {
			name = strings.TrimSpace(*c.Name)
		}
		if name == "" {
			name = jidLocal(jid)
		}
		if taken[name] {
			// Re-check the suffixed candidate too: two JIDs can share both a
			// display name and a local part (e.g. "…@s.whatsapp.net" vs
			// "…@lid"), and a three-way collision must never merge two chats'
			// conversations. A numeric suffix keeps later ones distinct while
			// staying deterministic (sorted-JID order).
			base := fmt.Sprintf("%s (%s)", name, jidLocal(jid))
			name = base
			for n := 2; taken[name]; n++ {
				name = fmt.Sprintf("%s #%d", base, n)
			}
		}
		taken[name] = true
		names[jid] = name
	}

	for _, jid := range jids {
		c, ok := chats[jid]
		if !ok {
			continue // skip-logged above
		}
		conv := Conversation{JID: jid, Name: names[jid], IsGroup: strings.HasSuffix(jid, groupSuffix)}
		conv.Messages = parseChatMessages(jid, conv.Name, conv.IsGroup, c, opts.ArchiveRoot, loc, skip)
		if len(conv.Messages) == 0 {
			// A chat whose messages all failed to parse (each already
			// skip-logged) or that carries none at all would persist an empty
			// conversation row; skip it instead.
			continue
		}
		if err := emit(conv); err != nil {
			return err
		}
	}
	return nil
}

// ParseAll collects every conversation into a slice along with the skip log
// (for tests and small archives).
func ParseAll(r io.Reader, opts ParseOptions) ([]Conversation, []ParseError, error) {
	var convs []Conversation
	var skips []ParseError
	err := Parse(r, opts,
		func(c Conversation) error { convs = append(convs, c); return nil },
		func(e ParseError) { skips = append(skips, e) },
	)
	return convs, skips, err
}

// parseChatMessages decodes, orders, and maps one chat's raw messages.
func parseChatMessages(
	jid, convName string, isGroup bool, c *chat,
	root string, loc *time.Location, skip func(ParseError),
) []signal.Message {
	type keyed struct {
		key string
		m   message
	}
	entries := make([]keyed, 0, len(c.Messages))
	for id, raw := range c.Messages {
		var m message
		if err := json.Unmarshal(raw, &m); err != nil {
			skip(ParseError{Chat: jid, MessageID: id, Err: fmt.Errorf("malformed message: %w", err)})
			continue
		}
		if m.Timestamp == nil || *m.Timestamp <= 0 {
			skip(ParseError{Chat: jid, MessageID: id, Err: fmt.Errorf("missing or invalid timestamp")})
			continue
		}
		if m.FromMe == nil {
			skip(ParseError{Chat: jid, MessageID: id, Err: fmt.Errorf("missing from_me")})
			continue
		}
		entries = append(entries, keyed{key: id, m: m})
	}

	// Chronological order, ties broken by numeric key (the exporter keys
	// messages by database row id, so this restores original order); Go maps
	// are unordered, so sorting is what makes re-parses deterministic.
	sort.Slice(entries, func(i, j int) bool {
		ti, tj := *entries[i].m.Timestamp, *entries[j].m.Timestamp
		if ti != tj {
			return ti < tj
		}
		ni, ei := strconv.ParseInt(entries[i].key, 10, 64)
		nj, ej := strconv.ParseInt(entries[j].key, 10, 64)
		if ei == nil && ej == nil && ni != nj {
			return ni < nj
		}
		return entries[i].key < entries[j].key
	})

	seq := newSeqCounter()
	msgs := make([]signal.Message, 0, len(entries))
	for _, e := range entries {
		msg := mapMessage(convName, isGroup, c.MediaBase, root, loc, &e.m)
		msg.Seq = seq.next(msg.Conversation, msg.TimestampRaw, msg.Sender, msg.Body)
		msgs = append(msgs, msg)
	}
	return msgs
}

// mapMessage converts one decoded exporter message into a signal.Message.
func mapMessage(convName string, isGroup bool, mediaBase, root string, loc *time.Location, m *message) signal.Message {
	// REQ-0009-004: canonical timestamp from the epoch field, never from the
	// exporter's pre-formatted strings. The wall clock is rendered in loc
	// (matching the local-time convention of the other sources); the parsed
	// Timestamp mirrors the other parsers: the raw string re-read as UTC,
	// purely a stable ordering key.
	wall := time.Unix(int64(*m.Timestamp), 0).In(loc)
	raw := wall.Format(signal.TimestampLayout)
	ts, _ := time.Parse(signal.TimestampLayout, raw)

	out := signal.Message{
		Conversation: convName,
		Timestamp:    ts,
		TimestampRaw: raw,
	}

	data := ""
	if m.Data != nil {
		data = *m.Data
	}

	// System/timeline events: the exporter marks these meta=true (group
	// renames, deleted messages, calls). meta is ALSO set on missing-media
	// and vCard rows, which stay regular messages — media distinguishes.
	// A data-less, media-less row is an unsupported internal message (the
	// exporter renders a placeholder for exactly this shape); it is kept as
	// an empty system event so timelines stay complete.
	if !m.Media && (m.Meta || m.Data == nil) {
		out.Sender = signal.SystemSender
		out.IsSystem = true
		out.Body = cleanBody(data)
		out.Links = signal.ExtractLinks(out.Body)
		out.Reactions = mapReactions(m.Reactions)
		return out
	}

	// Sender resolution: from_me is the archive owner; group messages carry
	// the member's resolved name (or number) in "sender"; 1:1 messages leave
	// sender null and belong to the chat's counterpart, i.e. the conversation
	// name. Group rows with a null sender (rare, content-less) fall back the
	// same way.
	switch {
	case *m.FromMe:
		out.Sender = signal.OwnerSender
	case m.Sender != nil && strings.TrimSpace(*m.Sender) != "":
		out.Sender = strings.TrimSpace(*m.Sender)
	default:
		out.Sender = convName
	}

	switch {
	case m.Media && (data == mediaMissingSentinel || data == ""):
		// Missing media: preserve an attachment entry with no path so the
		// transcript's chip fallback renders it; the sentinel is exporter
		// prose, not user content, so it never becomes body text.
		out.Attachments = []signal.Attachment{{
			Kind: signal.KindFile, RelPath: "", OriginalName: mediaMissingSentinel,
		}}
		out.Body = caption(m)
	case m.Media && m.Mime != nil && *m.Mime == vcardMime:
		// Contact card: data is an HTML summary with anchors to extracted
		// .vcf files. Anchors become file attachments (labels are the
		// contact names); the remaining prose is kept as plain text.
		body, atts := parseVCard(data, mediaBase, root)
		out.Body = body
		out.Attachments = atts
	case m.Media && data != "":
		// Regular media: full path = media_base + data (the exporter's HTML
		// template concatenates them via <base href>); store it root-relative.
		rel := relativizeMedia(mediaBase, data, root)
		kind := signal.KindFile
		if m.Mime != nil {
			mime := *m.Mime
			if strings.HasPrefix(mime, "image/") {
				kind = signal.KindImage
			} else if strings.HasPrefix(mime, "video/") {
				kind = signal.KindVideo
			}
		}
		out.Attachments = []signal.Attachment{{
			Kind: kind, RelPath: rel, OriginalName: filepath.Base(rel),
		}}
		out.Body = caption(m)
	default:
		// Plain text (quoted_data/reply deliberately never merge into the
		// body — the quoted parent is its own message).
		out.Body = cleanBody(data)
	}

	out.Links = signal.ExtractLinks(out.Body)
	out.Reactions = mapReactions(m.Reactions)
	return out
}

// caption returns a media message's caption (if any) as its body text.
func caption(m *message) string {
	if m.Caption == nil {
		return ""
	}
	return cleanBody(*m.Caption)
}

// cleanBody undoes the exporter's newline substitution ("<br>" → "\n") and
// collapses any exporter-injected anchors to their labels so markup never
// becomes body text. It deliberately does NOT tag-strip or entity-unescape
// general text: the exporter stores user text raw, so aggressive stripping
// would corrupt legitimate messages containing < and >.
func cleanBody(s string) string {
	if s == "" {
		return ""
	}
	s = brRe.ReplaceAllString(s, "\n")
	if strings.Contains(s, "<a href") {
		s = anchorRe.ReplaceAllString(s, "$2")
	}
	return strings.TrimSpace(s)
}

// parseVCard maps a vCard summary ("This media include the following vCard
// file(s):<br><a href="…/Name.vcf">Name</a> | …") to a plain-text body plus
// one file attachment per anchor. Exporter HTML never reaches the body.
func parseVCard(data, mediaBase, root string) (string, []signal.Attachment) {
	var atts []signal.Attachment
	for _, m := range anchorRe.FindAllStringSubmatch(data, -1) {
		href, label := html.UnescapeString(m[1]), strings.TrimSpace(html.UnescapeString(m[2]))
		if href == "" {
			continue
		}
		name := label
		if name == "" {
			name = filepath.Base(href)
		}
		atts = append(atts, signal.Attachment{
			Kind:         signal.KindFile,
			RelPath:      relativizeMedia("", href, root),
			OriginalName: name,
		})
	}
	// The summary is exporter-generated HTML (labels/paths are escaped with
	// html.escape), so a full clean is safe here: anchors collapse to their
	// labels, residual tags are stripped, and entities are unescaped.
	body := brRe.ReplaceAllString(data, "\n")
	body = anchorRe.ReplaceAllString(body, "$2")
	body = tagRe.ReplaceAllString(body, "")
	return strings.TrimSpace(html.UnescapeString(body)), atts
}

// relativizeMedia turns the exporter's media reference (media_base + data)
// into an archive-root-relative path. media_base may be "" (Android), a
// relative prefix, or an ABSOLUTE prefix (when the exporter ran with an
// absolute output dir); stored RelPaths must be root-relative only, so:
//
//  1. an absolute full path under root is relativized against it;
//  2. an absolute full path elsewhere falls back to the relative data part
//     (the exporter writes data relative to its output dir on iOS);
//  3. a still-absolute last resort keeps only the basename — a chip that
//     404s beats persisting a foreign absolute path.
//
// Relative results are cleaned; serving containment (traversal rejection) is
// archivepath.Contain's job at request time.
func relativizeMedia(mediaBase, data, root string) string {
	full := mediaBase + data
	if !filepath.IsAbs(full) {
		return filepath.ToSlash(filepath.Clean(full))
	}
	if root != "" {
		if rel, err := filepath.Rel(root, full); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(filepath.Clean(rel))
		}
	}
	if !filepath.IsAbs(data) {
		return filepath.ToSlash(filepath.Clean(data))
	}
	return filepath.Base(data)
}

// mapReactions converts the exporter's {actor: emoji} object into
// signal.Reactions, deterministically ordered by actor. The exporter names
// the archive owner "You"; that maps to signal.OwnerSender so reaction badges
// attribute consistently across sources.
func mapReactions(r map[string]string) []signal.Reaction {
	if len(r) == 0 {
		return nil
	}
	actors := make([]string, 0, len(r))
	for a := range r {
		actors = append(actors, a)
	}
	sort.Strings(actors)
	out := make([]signal.Reaction, 0, len(actors))
	for _, a := range actors {
		emoji := strings.TrimSpace(r[a])
		if emoji == "" {
			continue
		}
		actor := a
		if actor == ownReactionActor {
			actor = signal.OwnerSender
		}
		out = append(out, signal.Reaction{Emoji: emoji, Actor: actor})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// jidLocal returns the local part of a JID ("1555…@s.whatsapp.net" → "1555…").
func jidLocal(jid string) string {
	if i := strings.Index(jid, "@"); i > 0 {
		return jid[:i]
	}
	return jid
}

// seqCounter assigns the per-conversation Seq disambiguator for byte-identical
// messages (mirrors the signal and imessage parsers' counters).
type seqCounter struct{ counts map[string]int }

func newSeqCounter() *seqCounter { return &seqCounter{counts: map[string]int{}} }

func (s *seqCounter) next(conv, tsRaw, sender, body string) int {
	key := conv + "\x00" + tsRaw + "\x00" + sender + "\x00" + body
	n := s.counts[key]
	s.counts[key] = n + 1
	return n
}
