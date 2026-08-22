package spam

import (
	"slices"
	"strings"
)

// Opt-out event types detected inside the message stream.
const (
	// EventStopSent is a bare STOP-style keyword you sent, standing alone.
	EventStopSent = "stop_sent"
	// EventNoticeSent is your canned DNC/TCPA notice.
	EventNoticeSent = "notice_sent"
)

// ManualEventTypes are the events that happen outside the message stream and
// can only be recorded by hand, with their confirmation numbers. msgbrowse
// never files any of them for you — `spam event add` records that you did.
var ManualEventTypes = []string{
	EventStopSent,
	EventNoticeSent,
	"reported_7726",
	"reported_apple_junk",
	"fcc_complaint_filed",
	"ftc_dnc_complaint_filed",
	"lawyer_referral",
}

// ValidEventType reports whether t is a recognized event type.
func ValidEventType(t string) bool { return slices.Contains(ManualEventTypes, t) }

// MatchOptOut classifies one OUTBOUND message body as an opt-out, returning
// EventStopSent, EventNoticeSent, or "".
//
// Two shapes count, and they are matched differently on purpose:
//
//   - A stop keyword must be the WHOLE normalized body. "STOP" is an opt-out;
//     "please stop sending me solar quotes, I already have panels" is a
//     complaint, and treating it as a formal opt-out would date the violation
//     window from the wrong message.
//   - The canned notice is matched as a normalized PREFIX of the configured
//     ratio, because autocorrect rewrites it and a long notice is often sent
//     truncated. The prefix has to appear anywhere in the body so a greeting
//     before it does not defeat the match.
func (r *Rules) MatchOptOut(body string) string {
	normalized := NormalizeForMatch(body)
	if normalized == "" {
		return ""
	}
	for _, kw := range r.StopKeywords {
		if normalized == NormalizeForMatch(kw) {
			return EventStopSent
		}
	}
	notice := NormalizeForMatch(r.CannedNotice)
	if notice == "" {
		return ""
	}
	// Cut on a rune boundary: a byte slice of a normalized notice can land
	// mid-codepoint on a non-ASCII name and never match anything.
	runes := []rune(notice)
	n := min(max(int(float64(len(runes))*r.NoticeMatchRatio), 1), len(runes))
	if strings.Contains(normalized, string(runes[:n])) {
		return EventNoticeSent
	}
	return ""
}
