package spam

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/store"
)

// windowDays is the rolling window the Do Not Call private right of action
// turns on: more than one contact from the same entity within ANY twelve
// months, not within the last twelve months specifically.
const windowDays = 365

// Window is the busiest run of inbound messages inside any windowDays span.
type Window struct {
	Count int    `json:"count"`
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
}

// Stats are the numbers a complaint form and a lawyer ask for.
type Stats struct {
	FirstSeen        string `json:"first_seen,omitempty"`
	LastSeen         string `json:"last_seen,omitempty"`
	Inbound          int    `json:"inbound"`
	Outbound         int    `json:"outbound"`
	Candidates       int    `json:"candidates"`
	OptOutAt         string `json:"opt_out_at,omitempty"`
	OptOutType       string `json:"opt_out_type,omitempty"`
	AfterOptOut      int    `json:"after_opt_out"`
	Trailing12Months int    `json:"trailing_12_months"`
	WorstWindow      Window `json:"worst_12_month_window"`
}

// MessageRecord is one message as it appears in a dossier: the body verbatim,
// its hash, and the additive fields extracted from it.
type MessageRecord struct {
	Timestamp     string   `json:"timestamp"`
	Direction     string   `json:"direction"`
	Sender        string   `json:"sender"`
	Body          string   `json:"body_verbatim"`
	BodySHA256    string   `json:"body_sha256"`
	MessageHash   string   `json:"message_hash"`
	Reasons       []string `json:"reasons,omitempty"`
	URLs          []string `json:"urls,omitempty"`
	Phones        []string `json:"callback_numbers,omitempty"`
	Emails        []string `json:"emails,omitempty"`
	NamesMatched  []string `json:"names_matched,omitempty"`
	Entities      []string `json:"entities_matched,omitempty"`
	IsAfterOptOut bool     `json:"is_after_optout"`
	// Missing marks a finding whose message no longer exists in the archive.
	Missing bool `json:"missing,omitempty"`
}

// EventRecord is one thing that happened to this sender.
type EventRecord struct {
	At      string `json:"at"`
	Type    string `json:"type"`
	Origin  string `json:"origin"`
	Details string `json:"details,omitempty"`
}

// SenderRecord is the counterparty and the judgments recorded about them.
type SenderRecord struct {
	Source           string `json:"source"`
	Identifier       string `json:"identifier"`
	ConversationName string `json:"conversation_name,omitempty"`
	Status           string `json:"status"`
	SuspectedEntity  string `json:"suspected_entity,omitempty"`
	ConsentStatus    string `json:"consent_status"`
	ConsentNotes     string `json:"consent_notes,omitempty"`
	Notes            string `json:"notes,omitempty"`
}

// Dossier is the exportable per-sender record. Markdown and JSON render from
// this one value, so the two formats cannot drift apart.
type Dossier struct {
	GeneratedAt    string          `json:"generated_at"`
	RulesetVersion string          `json:"ruleset_version"`
	Sender         SenderRecord    `json:"sender"`
	Stats          Stats           `json:"stats"`
	Events         []EventRecord   `json:"events"`
	Messages       []MessageRecord `json:"messages"`
	// Limitations are the things this record cannot establish. They are part of
	// the artifact, not a footnote in the docs: handing a lawyer a dossier that
	// silently implies a precision msgbrowse does not have is the one failure
	// mode worth engineering against.
	Limitations []string `json:"limitations"`
	// Provenance is the distinct scan environments that produced this record's
	// findings — the contacts.Availability string each row was written under,
	// with "" meaning a row that predates schemaV20 and never recorded one.
	//
	// More than one entry means a MIXED record: some messages were selected
	// because the sender was absent from a readable address book, others only
	// because the thread name looked like a bare handle. Limitations says so
	// explicitly; this field is what lets a reader check the claim.
	Provenance []string `json:"provenance,omitempty"`
}

// BuildDossier assembles the record for one sender.
func BuildDossier(ctx context.Context, st *store.Store, sender store.SpamSender, version string, now time.Time) (Dossier, error) {
	msgs, err := st.SpamMessages(ctx, sender.Source, sender.Identifier, version)
	if err != nil {
		return Dossier{}, err
	}
	events, err := st.ListSpamEvents(ctx, sender.Source, sender.Identifier)
	if err != nil {
		return Dossier{}, err
	}
	provenance, err := st.SpamProvenance(ctx, sender.Source, sender.Identifier, version)
	if err != nil {
		return Dossier{}, err
	}

	d := Dossier{
		GeneratedAt:    now.UTC().Format(time.RFC3339),
		RulesetVersion: version,
		Sender: SenderRecord{
			Source:           sender.Source,
			Identifier:       sender.Identifier,
			ConversationName: sender.ConversationName,
			Status:           sender.Status,
			SuspectedEntity:  sender.SuspectedEntity,
			ConsentStatus:    sender.ConsentStatus,
			ConsentNotes:     sender.ConsentNotes,
			Notes:            sender.Notes,
		},
		Limitations: Limitations(provenance),
		Provenance:  provenance,
	}

	var inbound []int64
	for _, m := range msgs {
		rec := MessageRecord{
			Timestamp:     m.TS,
			Direction:     m.Direction,
			Sender:        m.Sender,
			Body:          m.Body,
			MessageHash:   m.MessageHash,
			Reasons:       m.Reasons,
			URLs:          m.URLs,
			Phones:        m.Phones,
			Emails:        m.Emails,
			NamesMatched:  m.Names,
			Entities:      m.Entities,
			IsAfterOptOut: m.IsAfterOptOut,
			Missing:       !m.Present,
		}
		if m.Present {
			sum := sha256.Sum256([]byte(m.Body))
			rec.BodySHA256 = hex.EncodeToString(sum[:])
		}
		if rec.Timestamp == "" {
			rec.Timestamp = time.Unix(m.TSUnix, 0).UTC().Format("2006-01-02 15:04:05")
		}
		d.Messages = append(d.Messages, rec)

		switch m.Direction {
		case store.SpamInbound:
			d.Stats.Inbound++
			inbound = append(inbound, m.TSUnix)
			if m.IsAfterOptOut {
				d.Stats.AfterOptOut++
			}
			if m.IsCandidate {
				d.Stats.Candidates++
			}
		case store.SpamOutbound:
			d.Stats.Outbound++
		}
	}

	for _, e := range events {
		d.Events = append(d.Events, EventRecord{
			At:      firstNonEmpty(e.EventAt, time.Unix(e.EventAtUnix, 0).UTC().Format(time.RFC3339)),
			Type:    e.EventType,
			Origin:  e.Origin,
			Details: e.Details,
		})
		if (e.EventType == EventStopSent || e.EventType == EventNoticeSent) && d.Stats.OptOutAt == "" {
			d.Stats.OptOutAt = e.EventAt
			d.Stats.OptOutType = e.EventType
		}
	}

	if sender.FirstSeenUnix > 0 {
		d.Stats.FirstSeen = time.Unix(sender.FirstSeenUnix, 0).UTC().Format("2006-01-02")
	}
	if sender.LastSeenUnix > 0 {
		d.Stats.LastSeen = time.Unix(sender.LastSeenUnix, 0).UTC().Format("2006-01-02")
	}
	d.Stats.Trailing12Months = Trailing12Months(inbound, now)
	d.Stats.WorstWindow = WorstWindow(inbound)
	return d, nil
}

// Trailing12Months counts inbound messages in the twelve months ending now.
func Trailing12Months(inbound []int64, now time.Time) int {
	cutoff := now.AddDate(-1, 0, 0).Unix()
	n := 0
	for _, ts := range inbound {
		if ts >= cutoff {
			n++
		}
	}
	return n
}

// WorstWindow returns the largest inbound count inside ANY twelve-month window.
//
// This is the number that matters, and it is not the same as the trailing
// twelve months: the DNC private right of action turns on more than one contact
// from the same entity within any twelve-month period, so a burst that ended
// eighteen months ago still counts. A two-pointer sweep over the sorted
// timestamps finds it in one pass.
func WorstWindow(inbound []int64) Window {
	if len(inbound) == 0 {
		return Window{}
	}
	ts := append([]int64(nil), inbound...)
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })

	span := int64(windowDays) * 24 * 3600
	best := Window{}
	lo := 0
	for hi := range ts {
		for ts[hi]-ts[lo] > span {
			lo++
		}
		if n := hi - lo + 1; n > best.Count {
			best = Window{
				Count: n,
				Start: time.Unix(ts[lo], 0).UTC().Format("2006-01-02"),
				End:   time.Unix(ts[hi], 0).UTC().Format("2006-01-02"),
			}
		}
	}
	return best
}

// availabilityAvailable is the one contacts.Availability value that means the
// scan had a real address book. It is a literal rather than a call to
// contacts.Available.String() so this file does not need the contacts import;
// TestAvailabilityAvailableMatchesContacts pins the two together so the
// duplication cannot silently drift.
const availabilityAvailable = "available"

// Governing: ADR-0029 (unsolicited-contact evidence)
// Implements: SPEC-0028 REQ-0028-013 "Scan-environment provenance", SPEC-0028 REQ-0028-011 "Dossier — one struct, two formats, and its own limitations"
//
// envIsDegraded reports whether a scan_env stamp describes a degraded scan.
//
// The stamp is "provider/availability" (spam.scanEnv). Only the availability
// half decides the stranger predicate, so a stamp is degraded whenever that
// half is anything other than "available". An unparseable or empty stamp is NOT
// degraded — it is unknown, which envIsUnknown handles separately; conflating
// the two would report a pre-schemaV20 row as a degraded scan, which asserts
// something nobody recorded.
func envIsDegraded(env string) bool {
	if env == "" {
		return false
	}
	_, availability, ok := strings.Cut(env, "/")
	if !ok {
		return false
	}
	return availability != availabilityAvailable
}

// Limitations is the fixed list of things a msgbrowse dossier cannot establish.
// It is embedded in every export.
//
// The first entry is the important one and it is specific to this
// implementation: msgbrowse reads exporter OUTPUT, not Apple's chat.db, and the
// imessage-exporter text format records a local wall-clock time with no UTC
// offset (internal/imessage/parser.go). "Exact timestamp with timezone" is an
// explicit evidentiary requirement, and this record does not meet it. Saying so
// is not a disclaimer; it is the difference between a useful organizing tool
// and a document that misleads the person relying on it.
func Limitations(provenance []string) []string {
	out := []string{
		"Timestamps are the archive's local wall-clock time with NO recorded UTC offset — the exporter does not preserve one. Any instant here can be off by the difference between the exporting machine's timezone and the reader's assumption, and a DST boundary can reorder two messages minutes apart. Do not present these as timezone-qualified timestamps.",
		"Message text comes from an exporter's output, not from Apple's chat.db. The body hash proves the text has not changed since msgbrowse imported it; it says nothing about the fidelity of the export itself.",
		"Carrier and line type (mobile / landline / VoIP) are NOT established. msgbrowse performs no carrier lookup, so the wireless-line element of a TCPA claim is unsupported by this record.",
		"Apple's message GUID and ROWID are not preserved by the text export, so a message here cannot be pointed back at a specific row in a fresh copy of chat.db.",
		"entities_matched is keyword and domain matching, not attribution. It is a lead to confirm by hand, never a finding.",
		"suspected_entity and consent_status are human judgments recorded by the archive owner, not derived from the messages.",
		"Group threads are not scanned; attachments and images are not part of this record.",
		"Nothing here is legal advice. It organizes facts; a lawyer decides what they mean.",
	}
	return append(out, provenanceLimitations(provenance)...)
}

// provenanceLimitations turns the scan environments behind a record into the
// limitations they imply (issue #385).
//
// A degraded scan has no address book, so it cannot tell a stranger from a
// friend whose thread is merely named by a bare number. It compensates by
// narrowing to phone/email-shaped thread names, which trades one error for
// another: senders you genuinely do not know are missed when their thread
// carries a display name. Either way the record is not the same evidence a
// contacts-backed scan produces, and a reader cannot infer that from the rows.
//
// The mixed case is called out separately because it is the one a reader is
// least likely to suspect: the record looks uniform, and the counts that make
// it persuasive are sums over two different selection rules.
func provenanceLimitations(provenance []string) []string {
	seen := map[string]bool{}
	for _, p := range provenance {
		seen[p] = true
	}
	var out []string
	if len(seen) > 1 {
		out = append(out, "This record MIXES scan environments: its findings were produced under more than one stranger predicate ("+strings.Join(sortedEnvs(seen), ", ")+"). Counts here are sums over rows selected by different rules and should not be read as a single consistent sample. Re-scan under one environment before relying on the totals.")
	}
	if seen[""] {
		out = append(out, "Some findings predate scan-environment recording, so it is not known whether an address book was readable when they were produced. Re-scan to stamp them.")
	}
	for _, env := range sortedKeys(seen) {
		if envIsDegraded(env) {
			out = append(out, "Findings were produced in DEGRADED mode (scan environment "+env+"): no address book could be read, so the scan could not tell a stranger from a known contact and narrowed to threads named by a bare phone number or email. Senders you do not know whose thread carries a display name are missing from this record, and a known contact whose thread is named by a bare number may be misfiled into it.")
			break
		}
	}
	return out
}

// sortedKeys gives provenanceLimitations a deterministic order, so a dossier
// built twice from the same rows renders identically.
func sortedKeys(seen map[string]bool) []string {
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedEnvs renders an environment set for a human, naming the unrecorded one
// rather than printing an empty string.
func sortedEnvs(seen map[string]bool) []string {
	out := make([]string, 0, len(seen))
	for e := range seen {
		if e == "" {
			e = "unrecorded"
		}
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// JSON renders the dossier as indented JSON.
func (d Dossier) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("spam: render dossier json: %w", err)
	}
	return append(b, '\n'), nil
}

// Markdown renders the dossier for a human. It reads from the same struct the
// JSON does, so the two cannot disagree about a fact.
func (d Dossier) Markdown() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("# Unsolicited-contact record — %s", d.Sender.Identifier)
	w("")
	w("Generated %s by msgbrowse (ruleset `%s`).", d.GeneratedAt, d.RulesetVersion)
	w("")
	w("## Sender")
	w("")
	w("| Field | Value |")
	w("| --- | --- |")
	w("| Identifier | `%s` |", d.Sender.Identifier)
	w("| Source | %s |", d.Sender.Source)
	if d.Sender.ConversationName != "" && d.Sender.ConversationName != d.Sender.Identifier {
		w("| Thread name in archive | %s |", mdCell(d.Sender.ConversationName))
	}
	w("| Status | %s |", d.Sender.Status)
	w("| Consent | %s |", d.Sender.ConsentStatus)
	if d.Sender.ConsentNotes != "" {
		w("| Consent notes | %s |", mdCell(d.Sender.ConsentNotes))
	}
	if d.Sender.SuspectedEntity != "" {
		w("| Suspected entity (unconfirmed) | %s |", mdCell(d.Sender.SuspectedEntity))
	}
	if d.Sender.Notes != "" {
		w("| Notes | %s |", mdCell(d.Sender.Notes))
	}
	w("| Carrier / line type | not established — msgbrowse performs no carrier lookup |")
	w("")

	w("## Summary")
	w("")
	if d.Stats.FirstSeen != "" {
		w("- **First seen:** %s", d.Stats.FirstSeen)
		w("- **Last seen:** %s", d.Stats.LastSeen)
	}
	w("- **Inbound messages:** %d (%d tripped a rule)", d.Stats.Inbound, d.Stats.Candidates)
	w("- **Outbound messages:** %d", d.Stats.Outbound)
	if d.Stats.OptOutAt != "" {
		w("- **Opt-out sent:** %s (%s)", d.Stats.OptOutAt, d.Stats.OptOutType)
		w("- **Inbound messages after the opt-out:** %d", d.Stats.AfterOptOut)
	} else {
		w("- **Opt-out sent:** none on record")
	}
	w("- **Inbound in the trailing 12 months:** %d", d.Stats.Trailing12Months)
	if d.Stats.WorstWindow.Count > 0 {
		w("- **Most inbound in any 12-month window:** %d (%s to %s)",
			d.Stats.WorstWindow.Count, d.Stats.WorstWindow.Start, d.Stats.WorstWindow.End)
	}
	w("")

	w("## Events")
	w("")
	if len(d.Events) == 0 {
		w("None recorded.")
	} else {
		w("| When | Type | Origin | Details |")
		w("| --- | --- | --- | --- |")
		for _, e := range d.Events {
			w("| %s | %s | %s | %s |", e.At, e.Type, e.Origin, mdCell(e.Details))
		}
	}
	w("")

	w("## Messages")
	w("")
	w("Bodies are quoted verbatim, exactly as imported. `body_sha256` is the SHA-256 of the quoted text.")
	w("")
	for _, m := range d.Messages {
		if m.Missing {
			w("### %s — %s (message no longer in the archive)", m.Timestamp, m.Direction)
			w("")
			w("A finding exists for message hash `%s`, but a later re-export removed the message. Recorded as a gap.", m.MessageHash)
			w("")
			continue
		}
		flag := ""
		if m.IsAfterOptOut {
			flag = " — **after opt-out**"
		}
		w("### %s — %s%s", m.Timestamp, m.Direction, flag)
		w("")
		for _, line := range strings.Split(m.Body, "\n") {
			w("> %s", line)
		}
		w("")
		w("- `body_sha256`: `%s`", m.BodySHA256)
		if len(m.Reasons) > 0 {
			w("- Rules tripped: %s", strings.Join(m.Reasons, ", "))
		}
		if len(m.URLs) > 0 {
			w("- Links: %s", strings.Join(m.URLs, ", "))
		}
		if len(m.Phones) > 0 {
			w("- Callback numbers: %s", strings.Join(m.Phones, ", "))
		}
		if len(m.Emails) > 0 {
			w("- Emails: %s", strings.Join(m.Emails, ", "))
		}
		if len(m.NamesMatched) > 0 {
			w("- Used your name: %s", strings.Join(m.NamesMatched, ", "))
		}
		if len(m.Entities) > 0 {
			w("- Entity leads (unconfirmed): %s", strings.Join(m.Entities, ", "))
		}
		w("")
	}

	w("## What this record cannot establish")
	w("")
	for _, l := range d.Limitations {
		w("- %s", l)
	}
	return b.String()
}

// mdCell makes a value safe to drop into a Markdown table cell: pipes would
// break the column, newlines would break the row.
func mdCell(v string) string {
	v = strings.ReplaceAll(v, "|", "\\|")
	return strings.Join(strings.Fields(v), " ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
