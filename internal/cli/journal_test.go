package cli

import "testing"

// TestJournalCommandSurface pins the journal command's stable CLI surface — its
// name and the four flags with their defaults — so the wiring to journal.Run
// (whose behavior is covered in internal/journal) can't silently drift.
func TestJournalCommandSurface(t *testing.T) {
	cmd := newJournalCommand()
	if cmd.Use != "journal" {
		t.Errorf("Use = %q, want %q", cmd.Use, "journal")
	}
	if cmd.RunE == nil {
		t.Fatal("journal command has no RunE (still stubbed?)")
	}
	for _, f := range []struct {
		name string
		def  string
	}{
		{"since", ""},
		{"backfill", "false"},
		{"regenerate", "false"},
		{"dry-run", "false"},
	} {
		fl := cmd.Flags().Lookup(f.name)
		if fl == nil {
			t.Errorf("missing flag --%s", f.name)
			continue
		}
		if fl.DefValue != f.def {
			t.Errorf("--%s default = %q, want %q", f.name, fl.DefValue, f.def)
		}
	}
}
