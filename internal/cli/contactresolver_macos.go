//go:build macoscontacts

// The macOS address-book seam for the CLI, selected only under
// `-tags macoscontacts`. macoscontacts.New self-degrades to "no address book"
// when the tag is present but cgo or the TCC grant is not, so wiring it here
// can never turn a working scan into a failing one.
//
// @joestump-agent 08/22/2026 - Added for `msgbrowse spam` (ADR-0029).
package cli

import (
	"log/slog"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/macoscontacts"
)

func newContactResolver(log *slog.Logger) contacts.Resolver { return macoscontacts.New(log) }
