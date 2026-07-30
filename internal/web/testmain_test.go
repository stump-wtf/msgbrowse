package web

import (
	"os"
	"testing"
)

// TestMain redirects HOME (USERPROFILE on Windows) to an empty throwaway
// directory for the whole test binary, so the default real-HOME detector
// (setup.NewDetector, reached lazily via Server.detector on every /providers
// render) probes a home with no Signal/iMessage/WhatsApp stores — the same
// situation Linux CI is in.
//
// Without this, any test that renders /providers on a developer Mac runs the
// real OS-permission probes, and ProbeWhatsAppContainer opens the actual
// WhatsApp group container (~/Library/Group Containers/
// group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite). That path is
// TCC-protected: in a shell where the consent flow cannot resolve (e.g. a
// sandboxed agent session) the open(2) blocks in-kernel indefinitely and the
// suite hangs to the panic timeout. stat(2) is permitted, so detection reports
// Detected and the probe proceeds to the blocking open — an empty HOME stops
// it one step earlier, at NotDetected. Tests that exercise specific probe
// states keep injecting faked detectors via SetDetector as before.
func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	home, err := os.MkdirTemp("", "msgbrowse-web-test-home-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(home)
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	return m.Run()
}
