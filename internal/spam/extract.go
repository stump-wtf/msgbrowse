// Package spam turns an already-imported message archive into an evidence
// record of unsolicited contact: which strangers messaged you, which of your
// rules each message tripped, when you told them to stop, and what arrived
// afterwards. It is the msgbrowse-native form of jonstump/spam-catcher
// (ADR-0029, SPEC-0028).
//
// Three properties are load-bearing. If you change this package, keep them
// true.
//
//   - **Nothing is sent anywhere.** Classification is deterministic, local, and
//     regex/keyword based. There is no LLM call and no lookup provider in this
//     package, so the ADR-0010 single-egress posture is unchanged: running
//     `msgbrowse spam scan` performs no network I/O at all.
//   - **The message body is never modified.** Every extracted field is
//     additive. The dossier quotes `messages.body` verbatim, exactly as the
//     importer stored it, and hashes that same text so a later alteration is
//     detectable.
//   - **Only strangers are recorded.** A sender the address book resolves, one
//     of your own numbers, or an allowlisted identifier never gets a
//     `spam_senders` row. When the address book cannot be read the scan does
//     NOT fall open and treat everyone as a stranger — it narrows to
//     phone/email-shaped conversation names and says so (see run.go).
//
// Nothing here is legal advice. It organizes facts; a lawyer decides what they
// mean.
//
// @joestump-agent 08/22/2026 - Initial port of the spam-catcher classification,
// opt-out detection, and dossier layers onto the msgbrowse store.
package spam

import (
	"regexp"
	"strings"
	"unicode"
)

// urlRe matches an explicit http(s)/www URL, or a bare host-with-path — which
// is how link shorteners usually appear in an SMS. It is deliberately looser
// than a validating URL parser: a lead that turns out to be noise costs a line
// in a dossier, a missed shortener costs the identifying detail.
var urlRe = regexp.MustCompile(`(?i)\b((?:https?://|www\.)[^\s<>"'\]\)]+|[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+/[^\s<>"'\]\)]*)`)

var emailRe = regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b`)

// phoneRe finds a NANP callback number inside a body: 555-123-4567,
// (555) 123 4567, +1 555 123 4567. Go's RE2 has no lookaround, so the
// "not glued to more digits" guard the Python original expressed as
// (?<![\d-]) … (?![\d-]) is applied by hand in Phones.
var phoneRe = regexp.MustCompile(`(?:\+?1[\s.-]*)?\(?\d{3}\)?[\s.-]*\d{3}[\s.-]*\d{4}`)

var nonDigit = regexp.MustCompile(`\D`)

// trailingPunct is stripped from the tail of a matched URL: senders end
// sentences, and "bit.ly/x." is not a host.
const trailingPunct = ".,;:!?)]}'\"“”’"

// URLs returns the distinct URLs in a body, in first-seen order.
func URLs(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range urlRe.FindAllStringSubmatch(body, -1) {
		u := strings.TrimRight(m[1], trailingPunct)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// DomainOf reduces a URL to its bare lowercase host, with any leading "www."
// removed, so a shortener denylist can be a plain set membership test.
func DomainOf(rawURL string) string {
	host := rawURL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host = strings.SplitN(host, "/", 2)[0]
	host = strings.SplitN(host, "?", 2)[0]
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	host = strings.SplitN(host, ":", 2)[0]
	host = strings.ToLower(host)
	return strings.TrimPrefix(host, "www.")
}

// Emails returns the distinct email addresses in a body, in first-seen order.
func Emails(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range emailRe.FindAllString(body, -1) {
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// Phones returns callback numbers found inside the body, normalized to E.164,
// in first-seen order. The sending number is frequently a burner while the
// callback number belongs to the actual operator, which is why this is a
// first-class extracted field and not a curiosity.
//
// Emails and URLs are blanked before scanning so an order number inside a
// tracking link is not mistaken for a phone number.
func Phones(body string) []string {
	text := emailRe.ReplaceAllString(body, " ")
	text = urlRe.ReplaceAllString(text, " ")

	var out []string
	seen := map[string]bool{}
	for _, loc := range phoneRe.FindAllStringIndex(text, -1) {
		if glued(text, loc[0]-1) || glued(text, loc[1]) {
			continue
		}
		digits := nonDigit.ReplaceAllString(text[loc[0]:loc[1]], "")
		var e164 string
		switch {
		case len(digits) == 10:
			e164 = "+1" + digits
		case len(digits) == 11 && strings.HasPrefix(digits, "1"):
			e164 = "+" + digits
		default:
			continue
		}
		if seen[e164] {
			continue
		}
		seen[e164] = true
		out = append(out, e164)
	}
	return out
}

// glued reports whether the byte at i (if in range) would extend a phone-number
// match — a digit or a hyphen. It stands in for the lookaround RE2 lacks.
func glued(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return (c >= '0' && c <= '9') || c == '-'
}

// NamesUsed reports which of the owner's name variants a message used, matched
// case-insensitively on word boundaries so "Jon" does not fire inside
// "Jonathan" unless "Jonathan" is itself a configured variant.
//
// A stranger using your first name is evidence they believe they know who they
// are texting, which is exactly the point a wrong-number defense turns on.
func NamesUsed(body string, variants []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range variants {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(v) + `\b`)
		if err != nil {
			continue
		}
		if re.MatchString(body) {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// Entities is a best-effort guess at the industry or business behind a message:
// configured keywords found in the text, plus the host of every link.
//
// It is keyword and domain matching, not attribution. Treat the result as a
// lead to confirm by hand, never as a finding — SPEC-0028 requires every
// surface that renders it to say so.
func Entities(body string, keywords, urls []string) []string {
	lower := strings.ToLower(body)
	var out []string
	seen := map[string]bool{}
	for _, kw := range keywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(kw)) && !seen[kw] {
			seen[kw] = true
			out = append(out, kw)
		}
	}
	for _, u := range urls {
		host := DomainOf(u)
		if host != "" && !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	return out
}

// AreaCode returns the NANP area code for a US/Canada number, or "" for
// anything else (a short code, an international number, an email handle).
func AreaCode(identifier string) string {
	digits := nonDigit.ReplaceAllString(identifier, "")
	switch {
	case len(digits) == 11 && strings.HasPrefix(digits, "1"):
		return digits[1:4]
	case len(digits) == 10:
		return digits[:3]
	}
	return ""
}

// NormalizeForMatch casefolds, strips punctuation, and collapses whitespace.
// It exists ONLY for opt-out matching, where autocorrect moves punctuation
// around and a trimmed send must still register. It is never applied to stored
// text: `spam_findings` indexes the body, it does not replace it.
func NormalizeForMatch(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteRune(' ')
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
