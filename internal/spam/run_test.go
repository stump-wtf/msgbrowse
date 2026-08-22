package spam

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// fakeBook is a readable address book holding exactly the identifiers given.
type fakeBook struct {
	avail contacts.Availability
	nums  []string
}

func (f fakeBook) Availability(context.Context) contacts.Availability { return f.avail }

func (f fakeBook) Resolve(ctx context.Context, id contacts.Identifier) ([]contacts.Person, error) {
	people, _ := f.People(ctx)
	var out []contacts.Person
	for _, p := range people {
		for _, pid := range p.Identifiers {
			if pid.Value == id.Value {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func (f fakeBook) People(context.Context) ([]contacts.Person, error) {
	var out []contacts.Person
	for _, n := range f.nums {
		id := contacts.Normalize(n)
		out = append(out, contacts.Person{Key: n, DisplayName: n, Identifiers: []contacts.Identifier{id}})
	}
	return out, nil
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seed(t *testing.T, st *store.Store, conv string, rows ...[3]string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.IMessage, conv)
	if err != nil {
		t.Fatal(err)
	}
	msgs := make([]signal.Message, 0, len(rows))
	for _, r := range rows {
		ts, err := time.Parse(signal.TimestampLayout, r[0])
		if err != nil {
			t.Fatalf("bad fixture timestamp %q: %v", r[0], err)
		}
		msgs = append(msgs, signal.Message{
			Conversation: conv, Timestamp: ts, TimestampRaw: r[0], Sender: r[1], Body: r[2],
		})
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.IMessage, msgs); err != nil {
		t.Fatal(err)
	}
	return id
}

func runScan(t *testing.T, st *store.Store, opts Options) Summary {
	t.Helper()
	if opts.Rules == nil {
		opts.Rules = testRules(t, nil)
	}
	sum, err := Run(context.Background(), st, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return sum
}

func TestScanRecordsStrangersAndSkipsContacts(t *testing.T) {
	st := newStore(t)
	seed(t, st, "+15551110001",
		[3]string{"2025-01-05 09:00:00", "+15551110001", "Hi Jon, cut your solar bill: https://bit.ly/x"},
		[3]string{"2025-01-06 09:00:00", "+15551110001", "still interested?"},
	)
	seed(t, st, "+14045559999",
		[3]string{"2025-01-05 09:00:00", "+14045559999", "dinner at 7?"},
	)

	sum := runScan(t, st, Options{
		AddressBook: fakeBook{avail: contacts.Available, nums: []string{"+14045559999"}},
	})
	if sum.SkippedInContact != 1 {
		t.Errorf("SkippedInContact = %d, want 1", sum.SkippedInContact)
	}
	if sum.Degraded {
		t.Error("run reported degraded with a readable address book")
	}
	// Both inbound messages come from a watched area code, so both are
	// candidates — the area-code rule fires on the sender, not the text.
	if sum.Senders != 1 || sum.Candidates != 2 {
		t.Fatalf("senders=%d candidates=%d, want 1/2", sum.Senders, sum.Candidates)
	}

	senders, err := st.ListSpamSenders(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(senders) != 1 || senders[0].Identifier != "+15551110001" {
		t.Fatalf("senders = %+v", senders)
	}
	// A rule fired, so the scan promotes seen → watch on its own.
	if senders[0].Status != store.SpamStatusWatch {
		t.Errorf("status = %q, want watch", senders[0].Status)
	}
	// consent_status defaults to "none on record", never "unknown".
	if senders[0].ConsentStatus != store.SpamConsentNone {
		t.Errorf("consent = %q", senders[0].ConsentStatus)
	}
}

func TestScanSkipsOwnNumberAndAllowlist(t *testing.T) {
	st := newStore(t)
	seed(t, st, "+15555550100", [3]string{"2025-01-05 09:00:00", "+15555550100", "note to self"})
	seed(t, st, "+18005551212", [3]string{"2025-01-05 09:00:00", "+18005551212", "Your code is 123456"})

	sum := runScan(t, st, Options{AddressBook: fakeBook{avail: contacts.Available}})
	if sum.SkippedOwner != 1 || sum.SkippedAllowlist != 1 {
		t.Fatalf("owner=%d allowlist=%d, want 1/1", sum.SkippedOwner, sum.SkippedAllowlist)
	}
	if sum.Senders != 0 {
		t.Errorf("recorded %d senders, want 0", sum.Senders)
	}
}

// Without a readable address book the scan must NOT treat every thread as a
// stranger — that would enroll everyone you know. It narrows to handle-shaped
// thread names and reports that it did.
func TestScanDegradesWithoutAddressBook(t *testing.T) {
	st := newStore(t)
	seed(t, st, "+15551110001", [3]string{"2025-01-05 09:00:00", "+15551110001", "hi Jon"})
	seed(t, st, "Mom", [3]string{"2025-01-05 09:00:00", "Mom", "call me"})

	sum := runScan(t, st, Options{}) // nil AddressBook → contacts.Unavailable
	if !sum.Degraded {
		t.Fatal("degraded not reported")
	}
	if sum.AddressBook != "absent" {
		t.Errorf("AddressBook = %q", sum.AddressBook)
	}
	if sum.SkippedNotShaped != 1 {
		t.Errorf("SkippedNotShaped = %d, want 1 (Mom)", sum.SkippedNotShaped)
	}
	if sum.Senders != 1 {
		t.Errorf("senders = %d, want 1", sum.Senders)
	}
}

func TestScanDetectsOptOutAndFlagsWhatFollows(t *testing.T) {
	st := newStore(t)
	seed(t, st, "+15551110001",
		[3]string{"2025-01-05 09:00:00", "+15551110001", "Hi Jon, solar quote?"},
		[3]string{"2025-01-05 09:30:00", signal.OwnerSender, "STOP"},
		[3]string{"2025-02-01 09:00:00", "+15551110001", "Following up on your solar quote"},
		[3]string{"2025-03-01 09:00:00", "+15551110001", "Last chance"},
	)

	sum := runScan(t, st, Options{AddressBook: fakeBook{avail: contacts.Available}})
	if sum.OptOutsDetected != 1 {
		t.Fatalf("OptOutsDetected = %d, want 1", sum.OptOutsDetected)
	}

	ctx := context.Background()
	rules := testRules(t, nil)
	msgs, err := st.SpamMessages(ctx, source.IMessage, "+15551110001", rules.Version())
	if err != nil {
		t.Fatal(err)
	}
	var after int
	for _, m := range msgs {
		if m.IsAfterOptOut {
			after++
		}
		if m.Direction == store.SpamOutbound && m.IsAfterOptOut {
			t.Error("an outbound message was flagged as arriving after the opt-out")
		}
	}
	if after != 2 {
		t.Errorf("after-opt-out = %d, want 2", after)
	}
}

// A manually recorded opt-out must re-flag messages scanned long before it —
// which is why the recompute is wholesale and never incremental.
func TestManualOptOutRecomputesRetroactively(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seed(t, st, "+15551110001",
		[3]string{"2025-01-05 09:00:00", "+15551110001", "solar quote?"},
		[3]string{"2025-06-05 09:00:00", "+15551110001", "still solar?"},
	)
	rules := testRules(t, nil)
	runScan(t, st, Options{Rules: rules, AddressBook: fakeBook{avail: contacts.Available}})

	msgs, _ := st.SpamMessages(ctx, source.IMessage, "+15551110001", rules.Version())
	for _, m := range msgs {
		if m.IsAfterOptOut {
			t.Fatal("flagged before any opt-out existed")
		}
	}

	at := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := st.AddSpamEvent(ctx, store.SpamEvent{
		Source: source.IMessage, Identifier: "+15551110001", EventType: EventNoticeSent,
		EventAt: at.Format(time.RFC3339), EventAtUnix: at.Unix(), Origin: "manual",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSpamAfterOptOut(ctx, rules.Version()); err != nil {
		t.Fatal(err)
	}

	msgs, _ = st.SpamMessages(ctx, source.IMessage, "+15551110001", rules.Version())
	var after int
	for _, m := range msgs {
		if m.IsAfterOptOut {
			after++
		}
	}
	if after != 1 {
		t.Fatalf("after-opt-out = %d, want 1 (only the June message)", after)
	}
}

func TestScanIsIdempotentAndIncremental(t *testing.T) {
	st := newStore(t)
	rules := testRules(t, nil)
	opts := Options{Rules: rules, AddressBook: fakeBook{avail: contacts.Available}}

	seed(t, st, "+15551110001", [3]string{"2025-01-05 09:00:00", "+15551110001", "hi Jon"})
	first := runScan(t, st, opts)
	if first.MessagesScanned != 1 {
		t.Fatalf("first scan examined %d", first.MessagesScanned)
	}

	// Re-running with no new messages must examine nothing and change nothing.
	second := runScan(t, st, opts)
	if second.MessagesScanned != 0 {
		t.Errorf("re-scan examined %d messages, want 0", second.MessagesScanned)
	}
	ctx := context.Background()
	msgs, _ := st.SpamMessages(ctx, source.IMessage, "+15551110001", rules.Version())
	if len(msgs) != 1 {
		t.Fatalf("findings duplicated: %d", len(msgs))
	}
}

// Changing any rule changes the ruleset version, and findings are stored per
// generation — so the old generation's rows must not answer a new query.
func TestRulesetVersionPartitionsFindings(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seed(t, st, "+16061110001", [3]string{"2025-01-05 09:00:00", "+16061110001", "plain text"})

	base := testRules(t, nil)
	runScan(t, st, Options{Rules: base, AddressBook: fakeBook{avail: contacts.Available}})
	msgs, _ := st.SpamMessages(ctx, source.IMessage, "+16061110001", base.Version())
	if len(msgs) != 1 || msgs[0].IsCandidate {
		t.Fatalf("baseline = %+v", msgs)
	}

	widened := testRules(t, func(r *Rules) { r.WatchAreaCodes = append(r.WatchAreaCodes, "606") })
	if widened.Version() == base.Version() {
		t.Fatal("widening the watch list did not move the version")
	}
	if old, _ := st.SpamMessages(ctx, source.IMessage, "+16061110001", widened.Version()); len(old) != 0 {
		t.Fatal("the new generation saw the old generation's findings")
	}

	runScan(t, st, Options{Rules: widened, AddressBook: fakeBook{avail: contacts.Available}})
	msgs, _ = st.SpamMessages(ctx, source.IMessage, "+16061110001", widened.Version())
	if len(msgs) != 1 || !msgs[0].IsCandidate {
		t.Fatalf("rescan under the widened ruleset = %+v", msgs)
	}
}

// --reset clears what a scan can re-derive and keeps what a person entered.
func TestResetKeepsHumanJudgments(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seed(t, st, "+15551110001", [3]string{"2025-01-05 09:00:00", "+15551110001", "hi Jon"})
	rules := testRules(t, nil)
	runScan(t, st, Options{Rules: rules, AddressBook: fakeBook{avail: contacts.Available}})

	tracked, notes := store.SpamStatusTracked, "used my name, wrong person"
	if err := st.SetSpamSenderFields(ctx, source.IMessage, "+15551110001", &tracked, nil, nil, nil, &notes); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	if err := st.AddSpamEvent(ctx, store.SpamEvent{
		Source: source.IMessage, Identifier: "+15551110001", EventType: "fcc_complaint_filed",
		EventAt: at.Format(time.RFC3339), EventAtUnix: at.Unix(), Details: "ticket 12345", Origin: "manual",
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.ResetSpam(ctx); err != nil {
		t.Fatal(err)
	}

	sender, err := st.GetSpamSender(ctx, source.IMessage, "+15551110001")
	if err != nil {
		t.Fatal(err)
	}
	if sender.Status != store.SpamStatusTracked || sender.Notes != notes {
		t.Errorf("reset destroyed human judgments: %+v", sender)
	}
	events, _ := st.ListSpamEvents(ctx, source.IMessage, "+15551110001")
	if len(events) != 1 || events[0].Details != "ticket 12345" {
		t.Errorf("reset destroyed a filed event: %+v", events)
	}
	if msgs, _ := st.SpamMessages(ctx, source.IMessage, "+15551110001", rules.Version()); len(msgs) != 0 {
		t.Errorf("reset kept %d derived findings", len(msgs))
	}
}

func TestScanHonorsExcludeList(t *testing.T) {
	st := newStore(t)
	seed(t, st, "+15551110001", [3]string{"2025-01-05 09:00:00", "+15551110001", "hi Jon"})
	sum := runScan(t, st, Options{
		AddressBook: fakeBook{avail: contacts.Available},
		Exclude:     []string{"+15551110001"},
	})
	if sum.Senders != 0 {
		t.Errorf("excluded conversation was scanned: %+v", sum)
	}
}

func TestScanTargetedUnknownConversationErrors(t *testing.T) {
	st := newStore(t)
	_, err := Run(context.Background(), st, Options{
		Rules:              testRules(t, nil),
		OnlyConversationID: 4242,
	})
	if err == nil || !strings.Contains(err.Error(), "not eligible") {
		t.Fatalf("err = %v, want a not-eligible error", err)
	}
}
