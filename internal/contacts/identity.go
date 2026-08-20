// Conversation Identity — What A Source Actually Knows About Its Counterparty
//
// Every importer used to hand the store one string: the conversation name. The
// store then wrote that same string into contact_identifiers as if it were a
// handle. For iMessage that works by accident (the conversation name usually IS
// a phone number or email), but for Signal it is a profile name and for
// WhatsApp a display name — so cross-source matching could never fire. On a
// real 2,429-contact archive that produced zero automatic merges, ever: not one
// of 115 Signal identifiers was phone- or email-shaped, so ReasonPhone and
// ReasonEmail had nothing to compare.
//
// This file separates the two things that string was doing:
//
//   - DisplayName — what the person is called. Always present.
//   - Identifier  — a real handle (phone / email) when the source has one.
//
// Sources that carry a real handle supply it (WhatsApp's JID local part is the
// phone number; an iMessage conversation named by a handle is one). Sources
// that genuinely do not — Signal exports are directories named by profile name,
// with no number anywhere in chat.md — fall back to the display name, which is
// then matched WEAKLY: it can suggest a merge but never perform one. See
// ReasonDisplayName in match.go.
//
// It also detects group threads. A comma-joined iMessage name
// ("me@…, chelsea@…") is a multi-recipient chat, not a person; minting a
// contact for it invents someone who does not exist.
//
// @joestump-agent 08/20/2026 - Added for issue #363.
package contacts

import "strings"

// SourceIdentity is what an importer knows about a conversation's counterparty.
// The zero value means "nothing beyond the name" and is safe: DeriveIdentity
// fills it in from the name alone.
type SourceIdentity struct {
	// DisplayName is what to show for this conversation. Never empty after
	// DeriveIdentity.
	DisplayName string
	// Identifier is the strongest real handle the source can supply — a phone
	// number or email in canonical form. Empty when the source has none, which
	// is the normal case for Signal.
	Identifier string
	// IdentifierKind classifies Identifier (KindPhone / KindEmail), or
	// KindHandle when the identity falls back to the display name.
	IdentifierKind Kind
	// IsGroup marks a multi-recipient thread. Group threads get no synthesized
	// contact: there is no single person behind them.
	IsGroup bool
}

// HasRealHandle reports whether the source supplied a phone or email rather
// than falling back to a display name. Only these identities can participate in
// the strong (auto-merging) match reasons.
func (si SourceIdentity) HasRealHandle() bool {
	return si.IdentifierKind == KindPhone || si.IdentifierKind == KindEmail
}

// DeriveIdentity works out the identity of a conversation from its name plus
// whatever extra the importer supplies via hint.
//
// hint.Identifier wins when it is phone- or email-shaped: an importer that
// parsed a real handle out of the export (WhatsApp's JID) knows better than the
// name does. Otherwise the name itself is tried as a handle, which is what
// makes iMessage work — its conversations are usually named by the handle. When
// neither yields a real handle the display name becomes the identifier, tagged
// KindHandle so the matcher treats it as weak evidence.
//
// hint.IsGroup is honoured when set (WhatsApp knows from the JID suffix); a
// comma-joined name is detected as a group regardless, which is how iMessage
// multi-recipient threads are caught.
func DeriveIdentity(name string, hint SourceIdentity) SourceIdentity {
	out := SourceIdentity{
		DisplayName: strings.TrimSpace(name),
		IsGroup:     hint.IsGroup || IsMultiRecipientName(name),
	}
	if out.DisplayName == "" {
		out.DisplayName = strings.TrimSpace(hint.DisplayName)
	}
	// A group thread has no counterparty identity to extract.
	if out.IsGroup {
		return out
	}
	// 1. The importer's parsed handle, when it really is one.
	if id := Normalize(hint.Identifier); id.Kind == KindPhone || id.Kind == KindEmail {
		out.Identifier, out.IdentifierKind = id.Value, id.Kind
		return out
	}
	// 2. The conversation name, when it is itself a handle (the iMessage case).
	if id := Normalize(out.DisplayName); id.Kind == KindPhone || id.Kind == KindEmail {
		out.Identifier, out.IdentifierKind = id.Value, id.Kind
		return out
	}
	// 3. Nothing real to key on (the Signal case). The display name becomes the
	//    identifier so the conversation still resolves to a stable contact, but
	//    it is KindHandle and therefore weak: it can suggest, never auto-merge.
	if out.DisplayName != "" {
		out.Identifier, out.IdentifierKind = out.DisplayName, KindHandle
	}
	return out
}

// multiRecipientSeparators are the separators exporters use when they name a
// thread by listing its participants. iMessage joins with ", ".
var multiRecipientSeparators = []string{", ", "; "}

// IsMultiRecipientName reports whether a conversation name is a participant
// LIST rather than one person's name — the shape iMessage gives a
// multi-recipient chat ("me@example.com, chelsea@example.com").
//
// The test is deliberately narrow: it requires a separator AND that at least
// two of the resulting parts are themselves real handles. A person legitimately
// named "Stump, Joe" (surname-first, as some address books export) has one part
// that is not a handle, so it is not mistaken for a group. Requiring handles
// rather than just counting commas is what keeps this from inventing groups out
// of ordinary names.
func IsMultiRecipientName(name string) bool {
	s := strings.TrimSpace(name)
	if s == "" {
		return false
	}
	for _, sep := range multiRecipientSeparators {
		if !strings.Contains(s, sep) {
			continue
		}
		handles := 0
		for _, part := range strings.Split(s, sep) {
			if id := Normalize(part); id.Kind == KindPhone || id.Kind == KindEmail {
				handles++
			}
		}
		if handles >= 2 {
			return true
		}
	}
	return false
}
