package spam

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// Governing: ADR-0029 (unsolicited-contact evidence)
// Implements: SPEC-0028 REQ-0028-013 "Scan-environment provenance", SPEC-0028 REQ-0028-003 "Ruleset version and generation partitioning"

// availabilityAvailable is duplicated in dossier.go so that file needs no
// contacts import. If contacts ever renames the value, the dossier would stop
// recognizing a healthy scan and would start labelling every record degraded —
// silently, because nothing else compares the two.
func TestAvailabilityAvailableMatchesContacts(t *testing.T) {
	if got := contacts.Available.String(); got != availabilityAvailable {
		t.Fatalf("contacts.Available.String() = %q, but dossier.go pins %q — update availabilityAvailable", got, availabilityAvailable)
	}
}

// The exclude list decides which conversations a generation covers at all, so
// two scans with different lists are not comparable. Before #385 it lived in
// Options and never reached computeVersion, which meant adding a conversation
// left its old findings in place under an unchanged version.
func TestExcludeListParticipatesInTheVersion(t *testing.T) {
	base := testRules(t, nil)
	excluded := testRules(t, func(r *Rules) { r.Exclude = []string{"+15551110001"} })

	if base.Version() == excluded.Version() {
		t.Fatalf("version %q unchanged by an exclude list — a change to it must re-derive", base.Version())
	}

	// Order is normalized, so the same policy spelled differently is the same
	// generation and does not force a pointless rescan.
	a := testRules(t, func(r *Rules) { r.Exclude = []string{"b", "a"} })
	b := testRules(t, func(r *Rules) { r.Exclude = []string{"a", "b"} })
	if a.Version() != b.Version() {
		t.Errorf("exclude list order changed the version: %q vs %q", a.Version(), b.Version())
	}
}

// A finding must record the predicate that produced it. Two scans of the same
// archive under different address-book availability write into the same
// generation, and without this column the rows are indistinguishable.
func TestFindingsRecordTheScanEnvironment(t *testing.T) {
	st := newStore(t)
	seed(t, st, "+15551110001",
		[3]string{"2025-01-05 09:00:00", "+15551110001", "cut your solar bill: https://bit.ly/x"},
	)
	rules := testRules(t, nil)
	runScan(t, st, Options{Rules: rules, AddressBook: fakeBook{avail: contacts.Available}})

	got, err := st.SpamProvenance(context.Background(), source.IMessage, "+15551110001", rules.Version())
	if err != nil {
		t.Fatalf("SpamProvenance: %v", err)
	}
	// fakeBook does not implement ProviderNamer, so the provider half is
	// "unknown" — the honest answer for a resolver that will not name itself.
	if len(got) != 1 || got[0] != "unknown/"+contacts.Available.String() {
		t.Fatalf("provenance = %v, want [%q]", got, "unknown/"+contacts.Available.String())
	}
}

// A degraded scan cannot tell a stranger from a friend whose thread is named by
// a bare number. The dossier has to say so — the same discipline the fixed
// limitations block already has.
func TestDossierFromDegradedRowsSaysSo(t *testing.T) {
	st := newStore(t)
	seed(t, st, "+15551110001",
		[3]string{"2025-01-05 09:00:00", "+15551110001", "cut your solar bill: https://bit.ly/x"},
	)
	rules := testRules(t, nil)
	// No address book: Run falls back to the shaped-thread heuristic.
	runScan(t, st, Options{Rules: rules, AddressBook: fakeBook{avail: contacts.Absent}})

	d := buildDossierFor(t, st, "+15551110001", rules.Version())

	if len(d.Provenance) != 1 || d.Provenance[0] != "unknown/"+contacts.Absent.String() {
		t.Fatalf("Provenance = %v, want [%q]", d.Provenance, "unknown/"+contacts.Absent.String())
	}
	if !hasLimitationContaining(d.Limitations, "DEGRADED") {
		t.Errorf("degraded dossier does not disclose degraded mode:\n%s", strings.Join(d.Limitations, "\n"))
	}
	// The disclosure must survive into the rendered artifact, not just the
	// struct — the Markdown is what a person actually reads.
	if !strings.Contains(d.Markdown(), "DEGRADED") {
		t.Error("Markdown render omits the degraded-mode limitation")
	}
}

// A contacts-backed scan must NOT carry the degraded disclosure, or the warning
// becomes noise that a reader learns to skip.
func TestDossierFromHealthyRowsHasNoDegradedLimitation(t *testing.T) {
	st := newStore(t)
	seed(t, st, "+15551110001",
		[3]string{"2025-01-05 09:00:00", "+15551110001", "cut your solar bill: https://bit.ly/x"},
	)
	rules := testRules(t, nil)
	runScan(t, st, Options{Rules: rules, AddressBook: fakeBook{avail: contacts.Available}})

	d := buildDossierFor(t, st, "+15551110001", rules.Version())
	if hasLimitationContaining(d.Limitations, "DEGRADED") {
		t.Errorf("healthy dossier claims degraded mode:\n%s", strings.Join(d.Limitations, "\n"))
	}
	if hasLimitationContaining(d.Limitations, "MIXES") {
		t.Errorf("single-environment dossier claims mixed provenance:\n%s", strings.Join(d.Limitations, "\n"))
	}
}

// The dangerous case: a record whose rows came from two different stranger
// predicates looks uniform, and its counts are sums over two selection rules.
func TestDossierSpanningBothPredicatesIsIdentifiable(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	rules := testRules(t, nil)
	version := rules.Version()

	conv := seed(t, st, "+15551110001",
		[3]string{"2025-01-05 09:00:00", "+15551110001", "first"},
	)
	sender := store.SpamSender{
		Source:           source.IMessage,
		Identifier:       "+15551110001",
		ConversationName: "+15551110001",
		FirstSeenUnix:    day(2025, time.January, 5),
		LastSeenUnix:     day(2025, time.January, 6),
	}
	// Same sender, same generation, two environments — exactly what a desktop
	// scan followed by a release-CLI scan produces.
	write := func(hash, env string, ts int64) {
		t.Helper()
		f := []store.SpamFinding{{
			MessageHash: hash,
			Source:      source.IMessage,
			Identifier:  "+15551110001",
			Direction:   "in",
			TSUnix:      ts,
			IsCandidate: true,
		}}
		if err := st.PutSpamBatch(ctx, conv, version, env, hash, sender, f, nil); err != nil {
			t.Fatalf("PutSpamBatch(%s): %v", env, err)
		}
	}
	write("hash-available", "macoscontacts/"+contacts.Available.String(), day(2025, time.January, 5))
	write("hash-absent", "none/"+contacts.Absent.String(), day(2025, time.January, 6))

	d := buildDossierFor(t, st, "+15551110001", version)

	if len(d.Provenance) != 2 {
		t.Fatalf("Provenance = %v, want both environments", d.Provenance)
	}
	if !hasLimitationContaining(d.Limitations, "MIXES") {
		t.Errorf("mixed-provenance dossier does not disclose it:\n%s", strings.Join(d.Limitations, "\n"))
	}
	if !strings.Contains(d.Markdown(), "MIXES") {
		t.Error("Markdown render omits the mixed-provenance limitation")
	}
}

// Rows written before schemaV20 carry no environment. That is unknown, not
// healthy, and must not be quietly presented as a contacts-backed scan.
func TestUnrecordedProvenanceIsReportedAsUnknown(t *testing.T) {
	got := Limitations([]string{""})
	if !hasLimitationContaining(got, "predate scan-environment recording") {
		t.Errorf("unrecorded provenance not disclosed:\n%s", strings.Join(got, "\n"))
	}
	if hasLimitationContaining(got, "MIXES") {
		t.Error("a single unrecorded environment is not a mixed record")
	}
}

// A record with no findings at all has nothing to disclose beyond the fixed
// list — an empty provenance must not synthesize a warning.
func TestEmptyProvenanceAddsNothing(t *testing.T) {
	if got, want := len(Limitations(nil)), len(Limitations([]string{"macoscontacts/" + availabilityAvailable})); got != want {
		t.Errorf("Limitations(nil) has %d entries, healthy has %d — they should match", got, want)
	}
}

func buildDossierFor(t *testing.T, st *store.Store, identifier, version string) Dossier {
	t.Helper()
	sender, err := st.GetSpamSender(context.Background(), source.IMessage, identifier)
	if err != nil {
		t.Fatalf("GetSpamSender: %v", err)
	}
	d, err := BuildDossier(context.Background(), st, sender, version, time.Date(2025, time.September, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildDossier: %v", err)
	}
	return d
}

func hasLimitationContaining(limits []string, substr string) bool {
	for _, l := range limits {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// The scan_env stamp is only as good as the provider half, and both shipped
// resolvers must name themselves or every real scan records "unknown".
func TestShippedResolversNameThemselves(t *testing.T) {
	var r contacts.Resolver = contacts.Unavailable{}
	n, ok := r.(ProviderNamer)
	if !ok {
		t.Fatal("contacts.Unavailable does not implement ProviderNamer")
	}
	if got := n.ProviderName(); got != "none" {
		t.Errorf("Unavailable.ProviderName() = %q, want none", got)
	}
	if got := scanEnv(r, contacts.Absent); got != "none/absent" {
		t.Errorf("scanEnv = %q, want none/absent", got)
	}
}

// A resolver that does not name itself must degrade to "unknown", never to a
// guess that would misattribute the record.
func TestUnnamedResolverStampsUnknown(t *testing.T) {
	if got := scanEnv(fakeBook{avail: contacts.Available}, contacts.Available); got != "unknown/available" {
		t.Errorf("scanEnv = %q, want unknown/available", got)
	}
}

// Only the availability half decides degraded; an unrecorded stamp is unknown,
// not degraded, or a pre-schemaV20 row would assert something never recorded.
func TestEnvIsDegradedReadsTheAvailabilityHalf(t *testing.T) {
	for env, want := range map[string]bool{
		"macoscontacts/available":        false,
		"macoscontacts/needs-permission": true,
		"none/absent":                    true,
		"":                               false,
		"malformed":                      false,
	} {
		if got := envIsDegraded(env); got != want {
			t.Errorf("envIsDegraded(%q) = %v, want %v", env, got, want)
		}
	}
}
