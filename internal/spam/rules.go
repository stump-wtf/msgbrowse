package spam

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/joestump/msgbrowse/internal/contacts"
)

// DefaultShortenerDomains is the starting denylist of link shorteners. A
// shortener in an unsolicited text is a stronger signal than a bare URL: it
// exists to hide the destination and to fingerprint the click.
var DefaultShortenerDomains = []string{
	"bit.ly", "tinyurl.com", "t.co", "goo.gl", "ow.ly", "is.gd", "buff.ly",
	"rebrand.ly", "cutt.ly", "shorturl.at", "rb.gy", "tiny.cc", "lnk.to",
}

// DefaultEntityKeywords are the industries that dominate unsolicited SMS. They
// are leads for the "who is behind this number" column, never findings.
var DefaultEntityKeywords = []string{
	"solar", "warranty", "insurance", "medicare", "loan", "refinance", "crypto",
	"investment", "debt", "timeshare", "roofing", "real estate", "clinic",
}

// DefaultStopKeywords are the bodies that, sent by you and standing alone,
// count as an opt-out.
var DefaultStopKeywords = []string{
	"STOP", "STOPALL", "UNSUBSCRIBE", "QUIT", "END", "CANCEL", "REVOKE",
}

// DefaultNoticeMatchRatio is how much of the canned notice must appear in an
// outbound message for it to register as one. Less than the whole string,
// because autocorrect and a trimmed send are the normal case.
const DefaultNoticeMatchRatio = 0.6

// Rules is the configured, normalized classification policy for one scan. It is
// built once per run by NewRules and is immutable afterwards, so the version
// stamp it carries cannot drift from the rules that actually ran.
type Rules struct {
	// MyNumbers are the owner's own identifiers, which are never counterparties.
	MyNumbers []string
	// Allowlist holds identifiers that are never candidates even though they
	// are not in the address book: banks, 2FA short codes, delivery notices.
	Allowlist []string
	// WatchAreaCodes are NANP area codes (no country code) worth flagging.
	WatchAreaCodes []string
	// NameVariants are the owner's first name and every misspelling a sender
	// has used for them.
	NameVariants []string
	// FlagAnyURL makes a bare link a reason on its own, not just a shortener.
	FlagAnyURL bool
	// ShortenerDomains is the link-shortener denylist.
	ShortenerDomains []string
	// EntityKeywords seed the industry guess.
	EntityKeywords []string
	// StopKeywords are the standalone opt-out bodies.
	StopKeywords []string
	// CannedNotice is the owner's DNC/TCPA notice, matched as a normalized
	// prefix of the given ratio.
	CannedNotice string
	// NoticeMatchRatio is the fraction of CannedNotice that must match.
	NoticeMatchRatio float64
	// Exclude is a denylist of conversation names never examined. It mirrors
	// spam.exclude_conversations and is applied before any body is read.
	//
	// It lives here, in the versioned ruleset, rather than in Options, because
	// it is POLICY: it decides which conversations a generation covers, so two
	// scans with different exclude lists are not comparable even though every
	// other rule matched. Keeping it in Options left it out of computeVersion,
	// which meant adding a conversation left its existing findings in place
	// under an unchanged version, and removing one added findings later under
	// that same version (issue #385).
	Exclude []string

	version    string
	allowKeys  map[string]struct{}
	myKeys     map[string]struct{}
	shortKeys  map[string]struct{}
	watchCodes map[string]struct{}
}

// NewRules normalizes a raw configuration into the ruleset a scan runs under
// and stamps it with a version.
//
// The version is a digest of the effective rules — the same recipe the journal
// uses for its prompt version. It is what makes a rule change behave like a
// model change elsewhere in msgbrowse: findings are stored per
// (message_hash, ruleset_version), so widening the watch list re-derives
// everything rather than leaving a database holding two incompatible
// generations of "candidate" with no way to tell them apart.
func NewRules(r Rules) (*Rules, error) {
	out := r
	if len(out.ShortenerDomains) == 0 {
		out.ShortenerDomains = append([]string(nil), DefaultShortenerDomains...)
	}
	if len(out.EntityKeywords) == 0 {
		out.EntityKeywords = append([]string(nil), DefaultEntityKeywords...)
	}
	if len(out.StopKeywords) == 0 {
		out.StopKeywords = append([]string(nil), DefaultStopKeywords...)
	}
	if out.NoticeMatchRatio <= 0 {
		out.NoticeMatchRatio = DefaultNoticeMatchRatio
	}
	if out.NoticeMatchRatio > 1 {
		out.NoticeMatchRatio = 1
	}

	out.MyNumbers = cleanList(out.MyNumbers)
	out.Allowlist = cleanList(out.Allowlist)
	out.WatchAreaCodes = cleanList(out.WatchAreaCodes)
	out.NameVariants = cleanList(out.NameVariants)
	out.ShortenerDomains = cleanList(out.ShortenerDomains)
	out.EntityKeywords = cleanList(out.EntityKeywords)
	out.StopKeywords = cleanList(out.StopKeywords)
	out.CannedNotice = strings.TrimSpace(out.CannedNotice)

	out.myKeys = keySet(out.MyNumbers)
	out.allowKeys = keySet(out.Allowlist)
	out.shortKeys = make(map[string]struct{}, len(out.ShortenerDomains))
	for _, d := range out.ShortenerDomains {
		out.shortKeys[strings.ToLower(strings.TrimPrefix(d, "www."))] = struct{}{}
	}
	out.watchCodes = make(map[string]struct{}, len(out.WatchAreaCodes))
	for _, c := range out.WatchAreaCodes {
		out.watchCodes[c] = struct{}{}
	}

	out.Exclude = sortedCopy(out.Exclude)

	v, err := out.computeVersion()
	if err != nil {
		return nil, err
	}
	out.version = v
	return &out, nil
}

// Version is the stamp every finding this ruleset produces carries.
func (r *Rules) Version() string { return r.version }

// Governing: ADR-0029 (unsolicited-contact evidence)
// Implements: SPEC-0028 REQ-0028-003 "Ruleset version and generation partitioning"
//
// computeVersion digests the effective rules. Only fields that change what a
// scan concludes participate: the notice text and ratio are in (they decide
// opt-outs), the entity keywords are in (they change an extracted column), and
// Exclude is in (it decides which conversations the generation covers at all).
//
// V moved 1 -> 2 when Exclude joined (issue #385). That intentionally
// invalidates every version computed before it, which is the point: findings
// derived under an unrecorded exclude list are not comparable to findings
// derived under a known one, and re-deriving is the only honest way to reconcile
// them.
//
// Address-book availability is deliberately absent. It changes what a scan
// concludes, but it is the scan ENVIRONMENT rather than policy, and hashing it
// here would re-derive the whole layer on every switch between the desktop app
// and the CLI. It is recorded per row instead — see schemaV20.
func (r *Rules) computeVersion() (string, error) {
	payload := struct {
		V          int      `json:"v"`
		My         []string `json:"my"`
		Allow      []string `json:"allow"`
		AreaCodes  []string `json:"area_codes"`
		Names      []string `json:"names"`
		AnyURL     bool     `json:"any_url"`
		Shorteners []string `json:"shorteners"`
		Entities   []string `json:"entities"`
		Stop       []string `json:"stop"`
		Notice     string   `json:"notice"`
		Ratio      float64  `json:"ratio"`
		Exclude    []string `json:"exclude"`
	}{
		V:          2,
		My:         sortedCopy(r.MyNumbers),
		Allow:      sortedCopy(r.Allowlist),
		AreaCodes:  sortedCopy(r.WatchAreaCodes),
		Names:      sortedCopy(r.NameVariants),
		AnyURL:     r.FlagAnyURL,
		Shorteners: sortedCopy(r.ShortenerDomains),
		Entities:   sortedCopy(r.EntityKeywords),
		Stop:       sortedCopy(r.StopKeywords),
		Notice:     NormalizeForMatch(r.CannedNotice),
		Ratio:      r.NoticeMatchRatio,
		Exclude:    sortedCopy(r.Exclude),
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("spam: version ruleset: %w", err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:12]), nil
}

// IsMine reports whether an identifier belongs to the archive owner.
func (r *Rules) IsMine(identifier string) bool {
	_, ok := r.myKeys[MatchKey(identifier)]
	return ok
}

// IsAllowlisted reports whether an identifier is exempt by configuration.
func (r *Rules) IsAllowlisted(identifier string) bool {
	_, ok := r.allowKeys[MatchKey(identifier)]
	return ok
}

// Classification is one message's scan result. Every field is additive: the
// message body is stored by the importer and read verbatim by the dossier, and
// nothing here replaces it.
type Classification struct {
	// IsCandidate is true when at least one rule fired. False is still recorded
	// — "this stranger messaged you and tripped nothing" is a fact the
	// rolling-window contact counts depend on.
	IsCandidate bool
	// Reasons names each rule that fired, e.g. "area_code:555",
	// "name_variant:Jon", "shortener:bit.ly", "url".
	Reasons []string
	URLs    []string
	Phones  []string
	Emails  []string
	Names   []string
	// Entities is a lead to confirm by hand, never an attribution.
	Entities []string
}

// Classify evaluates one inbound message from a known stranger.
//
// The caller has already decided the sender is a stranger (see run.go's
// stranger predicate); this function only asks which rules the content trips.
// A non-match is not a discard: the zero-reason Classification is stored so the
// sender accumulates history and can be promoted by hand later.
func (r *Rules) Classify(identifier, body string) Classification {
	urls := URLs(body)
	c := Classification{
		URLs:   urls,
		Phones: Phones(body),
		Emails: Emails(body),
		Names:  NamesUsed(body, r.NameVariants),
	}
	c.Entities = Entities(body, r.EntityKeywords, urls)

	if code := AreaCode(identifier); code != "" {
		if _, ok := r.watchCodes[code]; ok {
			c.Reasons = append(c.Reasons, "area_code:"+code)
		}
	}
	if len(c.Names) > 0 {
		c.Reasons = append(c.Reasons, "name_variant:"+strings.Join(c.Names, ","))
	}

	var shorteners []string
	seen := map[string]bool{}
	for _, u := range urls {
		host := DomainOf(u)
		if _, ok := r.shortKeys[host]; ok && !seen[host] {
			seen[host] = true
			shorteners = append(shorteners, host)
		}
	}
	switch {
	case len(shorteners) > 0:
		c.Reasons = append(c.Reasons, "shortener:"+strings.Join(sortedCopy(shorteners), ","))
	case len(urls) > 0 && r.FlagAnyURL:
		c.Reasons = append(c.Reasons, "url")
	}

	c.IsCandidate = len(c.Reasons) > 0
	return c
}

// MatchKey reduces an identifier to the form two spellings of the same
// counterparty share.
//
// For phone-shaped input it is the trailing ten digits, so "+15551234567",
// "(555) 123-4567" and "5551234567" all collide — the same widening
// internal/contacts documents for cross-provider phone matching, and the rule
// spam-catcher applied when comparing against Contacts. Emails fold to
// lowercase. Anything else is the trimmed original: service handles are
// case-sensitive on their service and must not be folded.
func MatchKey(raw string) string {
	id := contacts.Normalize(raw)
	switch id.Kind {
	case contacts.KindPhone:
		digits := nonDigit.ReplaceAllString(id.Value, "")
		if len(digits) > 10 {
			return digits[len(digits)-10:]
		}
		return digits
	case contacts.KindEmail:
		return id.Value
	default:
		// Short codes (fewer than 7 digits) are not phone-shaped to
		// NormalizePhone, so they land here as handles. Compare them on their
		// digits so "662265" matches however it was written.
		trimmed := strings.TrimSpace(raw)
		if digits := nonDigit.ReplaceAllString(trimmed, ""); digits != "" && digits == strings.TrimPrefix(trimmed, "+") {
			return digits
		}
		return trimmed
	}
}

func cleanList(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func keySet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		if k := MatchKey(s); k != "" {
			out[k] = struct{}{}
		}
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
