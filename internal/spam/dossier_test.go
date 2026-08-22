package spam

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

func day(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Unix()
}

// The worst window is NOT the trailing twelve months. The DNC private right of
// action turns on more than one contact within ANY twelve-month period, so a
// burst that ended long ago still counts — getting this wrong understates the
// only number that matters.
func TestWorstWindowFindsAnOldBurst(t *testing.T) {
	inbound := []int64{
		day(2022, time.January, 10),
		day(2022, time.February, 10),
		day(2022, time.March, 10),
		day(2022, time.April, 10),
		day(2025, time.August, 1),
	}
	got := WorstWindow(inbound)
	if got.Count != 4 {
		t.Fatalf("Count = %d, want 4", got.Count)
	}
	if got.Start != "2022-01-10" || got.End != "2022-04-10" {
		t.Errorf("window = %s..%s", got.Start, got.End)
	}

	now := time.Date(2025, time.September, 1, 0, 0, 0, 0, time.UTC)
	if n := Trailing12Months(inbound, now); n != 1 {
		t.Errorf("Trailing12Months = %d, want 1", n)
	}
}

func TestWorstWindowExcludesMessagesOutsideTheSpan(t *testing.T) {
	inbound := []int64{
		day(2023, time.January, 1),
		day(2024, time.January, 5), // 369 days later — outside a 365-day window
	}
	if got := WorstWindow(inbound); got.Count != 1 {
		t.Errorf("Count = %d, want 1", got.Count)
	}
	if got := WorstWindow(nil); got.Count != 0 || got.Start != "" {
		t.Errorf("empty input = %+v", got)
	}
}

// Order must not matter: the sweep sorts first, and findings come back in
// ts_unix order but events and re-ingests can reshuffle equal timestamps.
func TestWorstWindowIsOrderIndependent(t *testing.T) {
	a := []int64{day(2024, time.May, 1), day(2024, time.January, 1), day(2024, time.March, 1)}
	b := []int64{day(2024, time.January, 1), day(2024, time.March, 1), day(2024, time.May, 1)}
	if WorstWindow(a) != WorstWindow(b) {
		t.Errorf("%+v != %+v", WorstWindow(a), WorstWindow(b))
	}
}

func seededDossier(t *testing.T) (Dossier, *Rules) {
	t.Helper()
	st := newStore(t)
	ctx := context.Background()
	seed(t, st, "+15551110001",
		[3]string{"2025-01-05 09:00:00", "+15551110001", "Hi Jon — solar quote at https://bit.ly/x, call 555-987-6543"},
		[3]string{"2025-01-05 09:30:00", signal.OwnerSender, "STOP"},
		[3]string{"2025-02-01 09:00:00", "+15551110001", "Following up"},
	)
	rules := testRules(t, nil)
	runScan(t, st, Options{Rules: rules, AddressBook: fakeBook{avail: contacts.Available}})

	tracked, entity := store.SpamStatusTracked, "solar lead broker"
	if err := st.SetSpamSenderFields(ctx, source.IMessage, "+15551110001", &tracked, &entity, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	sender, err := st.GetSpamSender(ctx, source.IMessage, "+15551110001")
	if err != nil {
		t.Fatal(err)
	}
	d, err := BuildDossier(ctx, st, sender, rules.Version(), time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return d, rules
}

func TestBuildDossierCarriesEvidence(t *testing.T) {
	d, rules := seededDossier(t)

	if d.RulesetVersion != rules.Version() {
		t.Errorf("ruleset = %q", d.RulesetVersion)
	}
	if d.Sender.Status != store.SpamStatusTracked || d.Sender.SuspectedEntity != "solar lead broker" {
		t.Errorf("sender = %+v", d.Sender)
	}
	if d.Stats.Inbound != 2 || d.Stats.Outbound != 1 {
		t.Errorf("counts = %+v", d.Stats)
	}
	if d.Stats.OptOutType != EventStopSent || d.Stats.AfterOptOut != 1 {
		t.Errorf("opt-out stats = %+v", d.Stats)
	}
	if len(d.Messages) != 3 {
		t.Fatalf("messages = %d", len(d.Messages))
	}

	first := d.Messages[0]
	if !strings.Contains(first.Body, "https://bit.ly/x") {
		t.Errorf("body was altered: %q", first.Body)
	}
	if first.BodySHA256 == "" || len(first.BodySHA256) != 64 {
		t.Errorf("body hash = %q", first.BodySHA256)
	}
	if len(first.Phones) != 1 || first.Phones[0] != "+15559876543" {
		t.Errorf("callback numbers = %q", first.Phones)
	}
	if len(d.Limitations) == 0 {
		t.Fatal("dossier shipped with no limitations section")
	}
}

// Markdown and JSON render from the same value, so they cannot disagree about
// a fact. This asserts the facts that matter appear in both.
func TestDossierFormatsAgree(t *testing.T) {
	d, _ := seededDossier(t)

	raw, err := d.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var round Dossier
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("dossier JSON does not round-trip: %v", err)
	}
	if round.Stats != d.Stats || round.Sender != d.Sender {
		t.Error("JSON lost a field")
	}

	md := d.Markdown()
	for _, want := range []string{
		d.Sender.Identifier,
		"solar lead broker",
		"https://bit.ly/x",
		d.Messages[0].BodySHA256,
		"after opt-out",
		"What this record cannot establish",
		"no carrier lookup",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

// The timezone gap is the most consequential thing a msgbrowse dossier cannot
// do, and it must be stated in the artifact itself — not only in the docs.
func TestDossierStatesTheTimezoneGap(t *testing.T) {
	d, _ := seededDossier(t)
	joined := strings.Join(d.Limitations, "\n")
	for _, want := range []string{"UTC offset", "carrier", "GUID", "legal advice"} {
		if !strings.Contains(joined, want) {
			t.Errorf("limitations do not mention %q", want)
		}
	}
	if !strings.Contains(d.Markdown(), "UTC offset") {
		t.Error("the rendered dossier does not state the timezone gap")
	}
}

func TestMarkdownEscapesTableCells(t *testing.T) {
	d := Dossier{Sender: SenderRecord{Identifier: "+1555", Notes: "a | b\nc"}}
	md := d.Markdown()
	if !strings.Contains(md, `a \| b c`) {
		t.Errorf("pipe/newline not neutralized in a table cell:\n%s", md)
	}
}
