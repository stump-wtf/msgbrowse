package web

import (
	"context"
	"net/url"
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
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	var id int64
	for _, p := range participants {
		if !strings.EqualFold(strings.TrimSpace(p.Name), name) {
			continue
		}
		if id != 0 && id != p.ContactID {
			return 0 // ambiguous: the same name belongs to two contacts today
		}
		id = p.ContactID
	}
	return id
}

// resolveDayCard fills the card's People and NotableLinks with their archive
// verdicts. Both reads are pure lookups; an empty day (no observed links, no
// participants) leaves every entry inert, which is the requirement's default.
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
	return nil
}
