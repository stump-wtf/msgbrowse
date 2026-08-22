//go:build !macoscontacts

// Default address-book seam for the CLI: no provider.
//
// The core binary is built CGO_ENABLED=0 and links no Contacts.framework
// symbols, so `msgbrowse spam scan` cannot ask macOS who is in your address
// book. It therefore runs in the degraded mode internal/spam documents —
// narrowing to phone/email-shaped threads and saying so in the run summary —
// rather than assuming everyone is a stranger.
//
// Build with `-tags macoscontacts` on macOS (with cgo) to wire the real
// provider; contactresolver_macos.go is the other half of this seam. The
// desktop shell already does this via internal/embedded.
//
// @joestump-agent 08/22/2026 - Added for `msgbrowse spam` (ADR-0029).
package cli

import (
	"log/slog"

	"github.com/joestump/msgbrowse/internal/contacts"
)

func newContactResolver(*slog.Logger) contacts.Resolver { return contacts.Unavailable{} }
