// Headless unit tests for the embedded-server wiring: these run with
// CGO_ENABLED=0 and no webview toolchain, which is how the desktop story is
// verified on machines that cannot open a window.
//
// Governing: ADR-0017, SPEC-0010 REQ "Embedded server on a loopback ephemeral
// port", REQ "Graceful shutdown".
package embedded

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/web"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{DataDir: t.TempDir()}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startServer starts an embedded server against a fresh data dir and
// registers cleanup that cancels it and waits for Close.
func startServer(t *testing.T, ctx context.Context, cancel context.CancelFunc) *Server {
	t.Helper()
	es, err := Start(ctx, testConfig(t), testLogger())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := es.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return es
}

// TestStartBindsLoopbackFixedPort verifies the desktop shell binds a fixed
// loopback port so the MCP endpoint URL stays stable across relaunches.
func TestStartBindsLoopbackFixedPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	es := startServer(t, ctx, cancel)

	u, err := url.Parse(es.URL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", es.URL, err)
	}
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		t.Errorf("URL = %q; want http://127.0.0.1:<port>", es.URL)
	}
	if u.Port() != "8789" {
		t.Errorf("URL port = %q; want 8789", u.Port())
	}
}

// TestServesRealAppOverLoopback proves zero handler divergence: GET / against
// the embedded server returns the same server-rendered shell, behind the same
// strict security headers, that `msgbrowse serve` produces.
func TestServesRealAppOverLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	es := startServer(t, ctx, cancel)

	resp, err := http.Get(es.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d; want 200", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q; want the strict policy from internal/web", csp)
	}
	if !strings.Contains(string(body), "msgbrowse") {
		t.Error("GET / did not render the app shell")
	}
}

// TestShutdownReleasesPortAndStore drives the full SPEC-0010 "Graceful
// shutdown" sequence headlessly: cancel the context (what window close does),
// wait for the serve loop, close the store, and confirm the loopback port and
// the SQLite database are both released.
func TestShutdownReleasesPortAndStore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := testConfig(t)
	es, err := Start(ctx, cfg, testLogger())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := "127.0.0.1:" + FixedPort

	cancel()
	select {
	case <-es.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("serve loop did not exit after context cancel")
	}
	if err := es.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Port released: we can bind the exact address again.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("loopback port not released after shutdown: %v", err)
	}
	ln.Close()

	// Store released: the database opens cleanly for a fresh consumer.
	st, err := store.Open(filepath.Join(cfg.DataDir, store.DBFileName))
	if err != nil {
		t.Fatalf("store not reopenable after shutdown: %v", err)
	}
	st.Close()
}

// TestEphemeralPortsDoNotCollide was originally written for the ephemeral
// port era; with a fixed port the test is verified to bind the same port and
// not conflict with the server.
func TestEphemeralPortsDoNotCollide(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	es := startServer(t, ctx, cancel)

	// The server holds the fixed port; confirm it's listening.
	resp, err := http.Get(es.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d; want 200", resp.StatusCode)
	}
}

// TestMCPEndpointSharesTheListener proves the menubar's MCP endpoint is real
// and honest: MCPURL is a path on the same loopback listener the webview uses
// (SPEC-0010 bind surface — no second listener), and a full MCP client
// session works against it over streamable HTTP.
func TestMCPEndpointSharesTheListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	es := startServer(t, ctx, cancel)

	if es.MCPURL != es.URL+MCPPath {
		t.Fatalf("MCPURL = %q; want %q on the same listener", es.MCPURL, es.URL+MCPPath)
	}

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: es.MCPURL}, nil)
	if err != nil {
		t.Fatalf("MCP connect over the embedded listener: %v", err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Error("no MCP tools served from the embedded endpoint")
	}
}

// TestMCPMountLeavesWebRoutesAlone guards against handler divergence: with
// the MCP handler mounted, "/" still renders the app shell through the strict
// security-header middleware, and only the exact MCPPath is diverted.
func TestMCPMountLeavesWebRoutesAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	es := startServer(t, ctx, cancel)

	resp, err := http.Get(es.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d; want 200", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q; want the strict policy — web middleware must survive the MCP mount", csp)
	}

	// A GET without an MCP session must not 404 (the route exists) and must
	// not render HTML (it is not the web app).
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, es.MCPURL, nil)
	mcpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", es.MCPURL, err)
	}
	defer mcpResp.Body.Close()
	if mcpResp.StatusCode == http.StatusNotFound {
		t.Errorf("GET %s = 404; the MCP handler is not mounted", es.MCPURL)
	}
	if ct := mcpResp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("GET %s Content-Type = %q; the MCP path must not fall through to the web app", es.MCPURL, ct)
	}
}

// TestHealthyReflectsServerState drives the status-line health source: true
// while the embedded server answers, false once shutdown has taken the serve
// loop down (SPEC-0010 "Status accuracy" — degraded when unhealthy).
func TestHealthyReflectsServerState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := testConfig(t)
	es, err := Start(ctx, cfg, testLogger())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !es.Healthy(context.Background()) {
		t.Error("Healthy = false while the embedded server is running")
	}

	cancel()
	select {
	case <-es.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("serve loop did not exit after context cancel")
	}
	if es.Healthy(context.Background()) {
		t.Error("Healthy = true after the serve loop exited")
	}
	if err := es.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDesktopChromeFollowsGOOS pins the #165 flag decision: only darwin (the
// hidden-title-bar shell) marks pages desktop-chrome; the Linux shell keeps
// its native title bar and needs no traffic-light inset.
func TestDesktopChromeFollowsGOOS(t *testing.T) {
	cases := map[string]bool{
		"darwin":  true,
		"linux":   false,
		"windows": false,
	}
	for goos, want := range cases {
		if got := desktopChromeFor(goos); got != want {
			t.Errorf("desktopChromeFor(%q) = %v, want %v", goos, got, want)
		}
	}
}

// TestWithShellNotesSurfacesOnLogsPage proves the #167 observability wiring
// end to end over the real loopback listener: notes handed to Start via
// WithShellNotes render on /logs (and an error note carries the failed
// badge), exactly what the owner will look at when the menubar misbehaves on
// hardware.
func TestWithShellNotesSurfacesOnLogsPage(t *testing.T) {
	notes := func() []web.ShellNote {
		return []web.ShellNote{
			{Time: time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC), Level: web.ShellNoteInfo, Message: "menubar: registering status item"},
			{Time: time.Date(2026, 7, 4, 10, 0, 30, 0, time.UTC), Level: web.ShellNoteError, Message: "menubar: status item did not register within 30s"},
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	es, err := Start(ctx, testConfig(t), testLogger(), WithShellNotes(notes))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := es.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	resp, err := http.Get(es.URL + "/logs")
	if err != nil {
		t.Fatalf("GET /logs: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /logs status = %d; want 200", resp.StatusCode)
	}
	page := string(body)
	if !strings.Contains(page, "Desktop shell") {
		t.Error("/logs missing the Desktop shell diagnostics section")
	}
	if !strings.Contains(page, "menubar: registering status item") {
		t.Error("/logs missing the info note")
	}
	if !strings.Contains(page, "menubar: status item did not register within 30s") {
		t.Error("/logs missing the error note")
	}
}

func TestResolveDataDir(t *testing.T) {
	// Relative paths (incl. the "./data" default) collapse to <base>/msgbrowse,
	// so a Finder launch (cwd="/") never tries to write /data. Absolute paths
	// pass through so an explicit CLI data dir is honored.
	base := filepath.Join("/Users", "someone", "Library", "Application Support")
	cases := []struct {
		in, want string
	}{
		{"./data", filepath.Join(base, "msgbrowse")},
		{"data", filepath.Join(base, "msgbrowse")},
		{"relative/nested", filepath.Join(base, "msgbrowse")},
		{"/explicit/abs", "/explicit/abs"},
	}
	for _, c := range cases {
		if got := resolveDataDir(c.in, base); got != c.want {
			t.Errorf("resolveDataDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
