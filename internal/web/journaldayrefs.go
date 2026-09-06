package web

import (
	"context"
	"net/url"
	"path"
	"strings"

	"github.com/joestump/msgbrowse/internal/store"
)

// Journal-day card link/person resolution (#371): the digest is LLM output
// derived from message content, which is attacker-influenceable — anyone who
// can text you can try to steer it. REQ-0016-016's original answer was to
// render notable links as inert text; the amended requirement keeps that
// defence and adds the affordance by MATCHING instead of FILTERING: a model
// string is only ever a lookup key against what the archive actually recorded
// for that day, and every href/person page comes from stored facts, never
// from the model. A hallucinated or injected entry has nothing to match and
// renders exactly as it did before — inert by construction.
//
// JournalLinkRef is one notable link as rendered. URL != "" means the model's
// Text matched a link observed on that day AND the STORED url re-passed the
// http/https allowlist (belt-and-braces on top of matching); the anchor then
// points at the stored URL while the display text stays the model's escaped
// string. URL == "" renders today's inert text.
type JournalLinkRef struct {
	Text string
	URL  string
}

// JournalPersonRef is one people chip as rendered. ContactID != 0 means the
// name resolved to EXACTLY ONE contact participating in that day's
// conversations, and links to their contact page. ContactID == 0 renders
// today's plain chip (absent or ambiguous never links).
type JournalPersonRef struct {
	Name      string
	ContactID int64
}

// normalizeLinkKey reduces a URL to the key notable-link matching compares:
// trimmed, parsed, scheme+host lowercased, default ports dropped, fragment
// dropped, trailing slash dropped. Anything unparseable matches nothing —
// model strings are lookup keys, not hrefs, so failure is safe.
func normalizeLinkKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if u.Scheme == "" || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Host)
	if h, port, ok := strings.Cut(host, ":"); ok {
		if port == "80" && u.Scheme == "http" || port == "443" && u.Scheme == "https" {
			host = h
		}
	}
	p := strings.TrimSuffix(u.EscapedPath(), "/")
	if p == "" {
		p = "/"
	}
	return strings.ToLower(u.Scheme) + "://" + host + p + "?" + u.RawQuery
}

// uniqueParticipant returns the single contact whose display name equals the
// digest's person name case-insensitively among the day's participants — or
// zero when absent, ambiguous (two different contacts share the name), or the
// name is itself empty. A group-less day resolves nothing.
func uniqueParticipant(name string, participants []store.JournalDayParticipant) int64 {
	key := participantKey(name)
	if key == "" {
		return 0
	}
	var id int64
	for _, p := range participants {
		if participantKey(p.Name) != key {
			continue
		}
		if id != 0 && id != p.ContactID {
			return 0 // ambiguous: the same name belongs to two contacts today
		}
		id = p.ContactID
	}
	return id
}

// participantKey is the comparison key for people-name resolution (#438):
// lowercased, whitespace-normalised. It makes an old digest's smooshed
// "ChelseaStump" resolve to the contact "Chelsea Stump" without rebuilding
// the digest. Ambiguity (the key matching two different contacts) is still
// handled by the caller.
func participantKey(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(humanName(name)), " "))
}

// resolveDayCard fills the card's People and NotableLinks with their archive
// verdicts. Both reads are pure lookups; an empty day (no observed links, no
// participants) leaves every entry inert, which is the requirement's default.
// JournalMediaRef is one standout-media entry as rendered (issue #439).
// Matched means the model's string resolved to a real attachment: images
// render as the gallery tile + :target lightbox pair, other kinds stay as a
// file chip linking to the source message. Unmatched (a hallucinated or
// re-import-renamed filename) stays today's inert chip.
type JournalMediaRef struct {
	Name           string // raw model string
	Matched        bool
	ID             int64
	MessageID      int64
	ConversationID int64
	Kind           string
	RelPath        string
	OriginalName   string
}

func (s *Server) resolveDayCard(ctx context.Context, day string, c *dayCard) error {
	if c == nil || c.Digest == nil {
		return nil
	}

	participants, err := s.store.JournalDayParticipants(ctx, day)
	if err != nil {
		return err
	}
	c.DigestPeople = make([]JournalPersonRef, 0, len(c.Digest.People))
	for _, name := range c.Digest.People {
		ref := JournalPersonRef{Name: name}
		if id := uniqueParticipant(name, participants); id != 0 {
			ref.ContactID = id
		}
		c.DigestPeople = append(c.DigestPeople, ref)
	}

	storedLinks, err := s.store.JournalDayLinks(ctx, day)
	if err != nil {
		return err
	}
	byKey := make(map[string]string, len(storedLinks))
	for _, l := range storedLinks {
		if k := normalizeLinkKey(l.URL); k != "" {
			// First stored occurrence wins; duplicates only ever differ in a
			// fragment already normalized away.
			if _, ok := byKey[k]; !ok {
				byKey[k] = l.URL
			}
		}
	}
	c.NotableLinks = make([]JournalLinkRef, 0, len(c.Digest.NotableLinks))
	for _, text := range c.Digest.NotableLinks {
		ref := JournalLinkRef{Text: text}
		if stored, ok := byKey[normalizeLinkKey(text)]; ok && validExternalURL(stored) {
			ref.URL = stored
		}
		c.NotableLinks = append(c.NotableLinks, ref)
	}

	// Standout media resolution (#439): match each model string against the
	// day's real attachments by basename(rel_path) or original_name,
	// case-insensitive — the same matching-not-filtering rule the people and
	// links lookups follow (the #371 rule). A string with no attachment match
	// stays an inert chip; denylisted conversations were never in the set.
	if len(c.Digest.StandoutMedia) > 0 {
		atts, aerr := s.store.DayAttachments(ctx, day, s.journalExclude)
		if aerr != nil {
			return aerr
		}
		byBase := make(map[string]store.DayAttachment, len(atts))
		byOrig := make(map[string]store.DayAttachment, len(atts))
		for _, a := range atts {
			byBase[strings.ToLower(path.Base(a.RelPath))] = a
			if a.OriginalName != "" {
				byOrig[strings.ToLower(a.OriginalName)] = a
			}
		}
		c.MediaRefs = make([]JournalMediaRef, 0, len(c.Digest.StandoutMedia))
		for _, raw := range c.Digest.StandoutMedia {
			ref := JournalMediaRef{Name: raw}
			want := strings.ToLower(strings.TrimSpace(raw))
			if want != "" {
				if a, ok := byBase[want]; ok {
					ref.Matched = true
					ref.ID, ref.MessageID, ref.ConversationID = a.ID, a.MessageID, a.ConversationID
					ref.Kind, ref.RelPath, ref.OriginalName = a.Kind, a.RelPath, a.OriginalName
				} else if a, ok := byOrig[want]; ok {
					ref.Matched = true
					ref.ID, ref.MessageID, ref.ConversationID = a.ID, a.MessageID, a.ConversationID
					ref.Kind, ref.RelPath, ref.OriginalName = a.Kind, a.RelPath, a.OriginalName
				}
			}
			c.MediaRefs = append(c.MediaRefs, ref)
		}
	}
	return nil
}
