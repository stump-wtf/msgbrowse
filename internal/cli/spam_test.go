package cli

import (
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/spf13/cobra"
)

func spamCommand(t *testing.T) *cobra.Command {
	t.Helper()
	for _, c := range NewRootCommand().Commands() {
		if c.Name() == "spam" {
			return c
		}
	}
	t.Fatal("the spam command is not registered on the root")
	return nil
}

func subcommand(t *testing.T, parent *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("%s has no %q subcommand", parent.Name(), name)
	return nil
}

// TestSpamCommandSurface pins the subcommand tree SPEC-0028 specifies. A
// renamed or missing verb here silently breaks the documented workflow.
func TestSpamCommandSurface(t *testing.T) {
	cmd := spamCommand(t)
	for _, name := range []string{"scan", "senders", "violations", "evidence", "sender-set", "event"} {
		subcommand(t, cmd, name)
	}
	event := subcommand(t, cmd, "event")
	subcommand(t, event, "add")
	subcommand(t, event, "list")
}

// The help text carries claims that are also commitments. "It reads and
// reports" and "no network egress" are the two the whole design rests on: if
// either stops being true, this test should be what fails.
func TestSpamCommandDocumentsItsPosture(t *testing.T) {
	long := spamCommand(t).Long
	for _, want := range []string{
		"reads and reports",
		"NO network egress",
		"Nothing here is legal advice",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("spam help does not state %q:\n%s", want, long)
		}
	}
}

// The degraded-address-book warning is the one thing a CLI user must not miss,
// because a quiet degraded run looks exactly like a clean one.
func TestSpamScanHelpExplainsTheAddressBookDependency(t *testing.T) {
	long := subcommand(t, spamCommand(t), "scan").Long
	for _, want := range []string{"address book", "macoscontacts", "ruleset version"} {
		if !strings.Contains(long, want) {
			t.Errorf("scan help does not mention %q", want)
		}
	}
}

func TestSpamEvidenceDefaultsToBothFormats(t *testing.T) {
	cmd := subcommand(t, spamCommand(t), "evidence")
	if f := cmd.Flags().Lookup("format"); f == nil || f.DefValue != "both" {
		t.Errorf("--format default = %v, want both", f)
	}
	if cmd.Flags().Lookup("identifier") == nil {
		t.Error("evidence has no --identifier flag")
	}
}

// Every field of the config block must reach the ruleset, or a user edits a key
// that quietly does nothing.
func TestRulesFromConfigCarriesEveryKey(t *testing.T) {
	cfg := config.SpamConfig{
		MyNumbers:              []string{"+15555550100"},
		Allowlist:              []string{"662265"},
		WatchAreaCodes:         []string{"555"},
		NameVariants:           []string{"Jon"},
		FlagAnyURL:             false,
		ShortenerDomains:       []string{"x.example"},
		EntityKeywords:         []string{"solar"},
		StopKeywords:           []string{"STOP"},
		CannedNotice:           "do not call",
		CannedNoticeMatchRatio: 0.8,
	}
	rules, err := rulesFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !rules.IsMine("+15555550100") || !rules.IsAllowlisted("662265") {
		t.Error("identity lists did not reach the ruleset")
	}
	if rules.FlagAnyURL || rules.NoticeMatchRatio != 0.8 {
		t.Errorf("scalars did not reach the ruleset: %+v", rules)
	}
	if rules.Classify("+15551110001", "hello").Reasons == nil {
		t.Error("watch_area_codes did not reach the ruleset")
	}

	// Changing one key must move the version, which is what forces a rescan.
	before := rules.Version()
	cfg.WatchAreaCodes = append(cfg.WatchAreaCodes, "606")
	after, err := rulesFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version() == before {
		t.Error("a config change did not move the ruleset version")
	}
}

func TestSpamStatusAndConsentValidation(t *testing.T) {
	if !validSpamStatus(store.SpamStatusTracked) || validSpamStatus("promoted") {
		t.Error("status validation is wrong")
	}
	if !validSpamConsent(store.SpamConsentRevoked) || validSpamConsent("maybe") {
		t.Error("consent validation is wrong")
	}
}

func TestSafeFilename(t *testing.T) {
	if got := safeFilename("+1 555/123"); got != "+1_555_123" {
		t.Errorf("safeFilename = %q", got)
	}
	if got := safeFilename("a@b.example"); got != "a_b.example" {
		t.Errorf("safeFilename = %q", got)
	}
}

// An unrecognized --format must be refused before any work happens. Validated
// only at the write, it fell through the --stdout branch's equality test and
// rendered Markdown — handing back a dossier in a format the caller did not ask
// for, which is worse than an error for a command whose output is evidence.
func TestValidateDossierFormat(t *testing.T) {
	for _, ok := range []string{"md", "json", "both"} {
		if err := validateDossierFormat(ok); err != nil {
			t.Errorf("validateDossierFormat(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"xml", "", "MD", "markdown"} {
		err := validateDossierFormat(bad)
		if err == nil {
			t.Errorf("validateDossierFormat(%q) accepted an unknown format", bad)
			continue
		}
		if !strings.Contains(err.Error(), "invalid --format") {
			t.Errorf("validateDossierFormat(%q) error does not name the flag: %v", bad, err)
		}
	}
}
