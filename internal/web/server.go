// Package web implements msgbrowse's server-rendered HTMX user interface.
//
// It is intentionally minimal: net/http with Go 1.22 pattern routing,
// html/template for rendering (which auto-escapes all message content), HTMX for
// partial updates, and a small amount of hand-written CSS. There is no SPA and no
// build step. The server binds to loopback by default and sets a strict
// Content-Security-Policy; message bodies are untrusted and always escaped.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/joestump/msgbrowse/internal/archivepath"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/devsync"
	"github.com/joestump/msgbrowse/internal/imageconv"
	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Store is the subset of *store.Store the web layer depends on. It is an
// interface so tests can wrap the real store — e.g. to prove the HTMX
// partial-render path never runs the sidebar listing (SPEC-0008 REQ-0008-006)
// — without a second storage implementation.
type Store interface {
	ListConversations(ctx context.Context) ([]store.ConversationSummary, error)
	ConversationRefs(ctx context.Context) ([]store.ConversationRef, error)
	ArchiveStats(ctx context.Context) (store.ArchiveStats, error)
	NewestMessageTS(ctx context.Context) (string, error)
	GetConversationByID(ctx context.Context, id int64) (*store.ConversationSummary, error)
	ConversationSourceName(ctx context.Context, id int64) (source, name string, err error)
	GetContactByID(ctx context.Context, contactID int64) (*store.Contact, error)
	ContactFacts(ctx context.Context, contactID int64) ([]store.ContactFact, error)
	ContactStats(ctx context.Context, contactID int64) (store.ContactStats, error)
	ContactMessageVolume(ctx context.Context, contactID int64) ([]store.MonthBucket, error)
	ContactMostActiveHour(ctx context.Context, contactID int64) (int, int, bool, error)
	ContactMostActiveWeekday(ctx context.Context, contactID int64) (string, bool, error)
	DayAttachments(ctx context.Context, day string, exclude []string) ([]store.DayAttachment, error)
	CountOrphanFacts(ctx context.Context) (int, error)
	ContactTopReactions(ctx context.Context, contactID int64, limit int) ([]store.EmojiCount, error)
	GetMessages(ctx context.Context, convID, cursorTSUnix, cursorID int64, limit int, desc bool) (*store.Page, error)
	GetContext(ctx context.Context, messageID int64, window int) ([]store.MessageView, error)
	MessageConversationID(ctx context.Context, messageID int64) (int64, bool, error)
	TogglePinned(ctx context.Context, convID int64) (bool, error)
	SearchMessages(ctx context.Context, opts store.SearchOptions) ([]store.SearchHit, error)
	CountMedia(ctx context.Context, f store.GalleryFilter) (store.MediaCounts, error)
	ListAttachments(ctx context.Context, kind string, f store.GalleryFilter, cursorTSUnix, cursorID int64) (*store.MediaPage, error)
	ListLinks(ctx context.Context, f store.GalleryFilter, cur store.LinkCursor) (*store.LinkPage, error)
	LatestIngestRun(ctx context.Context) (*store.IngestRun, error)
	ListSnapshots(ctx context.Context) ([]store.Snapshot, error)
	LatestJournalDay(ctx context.Context) (string, error)
	LatestJournalDayInYear(ctx context.Context, year int) (string, bool, error)
	JournalYears(ctx context.Context) ([]int, error)
	JournalMonth(ctx context.Context, year int, month time.Month) ([]store.JournalMonthDay, error)
	MonthTopReactions(ctx context.Context, year int, month time.Month, exclude []string, perDay int) (map[string][]store.EmojiCount, error)
	JournalStats(ctx context.Context, year int, exclude []string) (store.JournalStats, error)
	GetJournalDay(ctx context.Context, day string) (store.JournalDayView, bool, error)
	// Day-card link/person resolution (#371): the archive facts the digest's
	// notable links and people chips are matched against before rendering.
	JournalDayLinks(ctx context.Context, day string) ([]store.JournalDayLink, error)
	JournalDayParticipants(ctx context.Context, day string) ([]store.JournalDayParticipant, error)
	// Home's two resurfacing cards (#239). Both are LIMITed and index-served so
	// they can run on the boosted #main-content path too (REQ-0008-006) — which
	// ListConversations deliberately cannot.
	MessageYearRange(ctx context.Context) (first, last int, err error)
	OnThisDayCandidates(ctx context.Context, days []string, perDay, limit int, exclude []string) ([]store.OnThisDayMessage, error)
	RecentConversations(ctx context.Context, n int) ([]store.RecentConversation, error)
	SourcesPresent(ctx context.Context) ([]string, error)
	SourceCounts(ctx context.Context) (map[string]store.SourceCount, error)
	LastSyncTimes(ctx context.Context) (map[string]time.Time, error)
	DeleteSourceData(ctx context.Context, src string) (int64, error)
	LatestEmbedRun(ctx context.Context) (*store.EmbedRun, error)
	RecentEmbedRuns(ctx context.Context, n int) ([]store.EmbedRun, error)
	// In-app journal build (#240).
	LatestJournalRun(ctx context.Context) (*store.JournalRun, error)
	RecentJournalRuns(ctx context.Context, n int) ([]store.JournalRun, error)
	JournalCoverage(ctx context.Context) (store.JournalCoverage, error)
	// The calendar's SECOND mood source (#370): sentiment-derived tints for days
	// the digest pass has not reached. See internal/web/journalmood.go for the
	// precedence and why the untinted state had to be given a name. The
	// per-contact opt-out is enforced inside MonthSentiment, not by this layer.
	LatestSentimentGeneration(ctx context.Context) (store.SentimentGeneration, bool, error)
	MonthSentiment(ctx context.Context, year int, month time.Month, gen store.SentimentGeneration, exclude []string) ([]store.SentimentDayConstruct, error)
	// In-app fact extraction (#366): the run log the Settings → Facts card reads,
	// the coverage snapshot behind it, and the per-contact cursor coverage that
	// lets the contact page tell "never extracted" from "extracted, no facts".
	LatestFactRun(ctx context.Context) (*store.FactRun, error)
	RecentFactRuns(ctx context.Context, n int) ([]store.FactRun, error)
	FactCoverage(ctx context.Context, exclude []string) (store.FactCoverage, error)
	ContactFactScan(ctx context.Context, contactID int64) (store.ContactFactScan, error)
	// In-app IPIP sentiment scoring (#367): the run log the Settings → Sentiment
	// card reads, the coverage snapshot behind it, and the generation-pinned
	// aggregates the three consumer surfaces (#313) fold. Every aggregate
	// enforces contact_sentiment_optout INSIDE its query rather than taking a
	// caller-supplied id list, so the privacy guarantee cannot be lost by a
	// caller that forgets to pass one; IsSentimentOptedOut is the SECOND check
	// the contact page makes, so an opted-out person gets no section at all
	// rather than an empty one inviting a score.
	LatestSentimentRun(ctx context.Context) (*store.SentimentRun, error)
	RecentSentimentRuns(ctx context.Context, n int) ([]store.SentimentRun, error)
	SentimentCoverage(ctx context.Context, gen store.SentimentGeneration, exclude []string) (store.SentimentCoverage, error)
	IsSentimentOptedOut(ctx context.Context, contactID int64) (bool, error)
	ContactScoredMessages(ctx context.Context, contactID int64, gen store.SentimentGeneration) (int, error)
	ContactSentimentMonths(ctx context.Context, contactID int64, gen store.SentimentGeneration) ([]store.SentimentBucketConstruct, error)
	ContactSentimentConstructs(ctx context.Context, contactID int64, gen store.SentimentGeneration) ([]store.SentimentBucketConstruct, error)
	DaySentiment(ctx context.Context, day string, gen store.SentimentGeneration, exclude []string) ([]store.SentimentBucketConstruct, error)
	SemanticSearch(ctx context.Context, query []float32, model string, opts store.SemanticOptions) ([]store.ScoredMessage, error)
	EmbeddingCoverage(ctx context.Context, model string) (store.EmbeddingCoverage, error)
	// Contact merge engine (#11) behind the Settings → Contacts tab (#12).
	GetMergeRules(ctx context.Context) (store.MergeRules, error)
	SetMergeRules(ctx context.Context, r store.MergeRules) error
	MergeCandidates(ctx context.Context, resolver contacts.Resolver) ([]store.MergeCandidate, error)
	ContactDiagnosticCounts(ctx context.Context) (store.ContactDiagnostics, error)
	MergedContacts(ctx context.Context) ([]store.MergedContact, error)
	MergeContacts(ctx context.Context, a, b int64) (int64, error)
	SplitContact(ctx context.Context, contactID int64, moved []store.ContactIdentifier) (int64, error)
}

// Server holds the dependencies shared by all handlers.
type Server struct {
	store Store
	// rootsCfg is the minimal config snapshot (archive roots + data dir) that
	// archiveRoots() resolves the per-source EFFECTIVE read-only archive roots
	// from: the configured cfg root when set, else the app-owned managed root
	// (<data_dir>/archives/<source>) when it exists on disk — issue #160 (the
	// desktop app imports into managed roots and sets no cfg root, so building
	// roots from cfg alone broke every /media resolve on desktop). Resolution is
	// per-call, not boot-time: the FIRST in-session Enable creates the managed
	// root after NewServer ran, and media must work without a relaunch.
	rootsCfg config.Config
	// cfgRoots are the EXPLICITLY configured archive roots only (no managed
	// fallback). They back sourceConfigured — the app-owned "Enabled" signal for
	// the Providers cards. The managed roots can exist as empty dirs before any
	// import (staging/adopt creates them), so an existing managed root must NOT
	// read as "Enabled"; store-presence is the desktop Enabled signal.
	cfgRoots   archivepath.Roots
	derivedDir string // cache of transcoded JPEGs (<data_dir>/derived)
	tmpl       *template.Template
	log        *slog.Logger
	mux        http.Handler
	staticTags map[string]string // embedded-static ETags, keyed by path within static/

	// deviceSyncEnabled mirrors config device_sync.enabled for the /settings
	// pairing section's absent state (SPEC-0010; payload contract SPEC-0011).
	deviceSyncEnabled bool
	// deviceSyncFeature reports whether the device-sync feature is compiled into
	// this binary at all — a COMPILE-TIME flag the shell sets from a
	// build-tagged constant (SetDeviceSyncFeature). Device sync is gated behind
	// the `devicesync` build tag and NOT built by default, so a release binary
	// hides the entire Device sync surface on /settings and /status. False (the
	// default) renders those sections as if the feature did not exist.
	deviceSyncFeature bool
	// pairing is the live pairing source for /settings' QR section and the
	// pair/unpair POSTs; nil until serve / the desktop shell wires
	// SetPairingSource.
	pairing PairingSource
	// syncMonitor is the device-sync state source (#158): live peer/folder
	// status for Settings + /status and the importer/replica role map behind
	// the Providers cards. nil (sync disabled / browser mode) renders the
	// labeled absent states. Wired via SetSyncMonitor.
	syncMonitor SyncMonitor
	// syncNotes supplies the device-sync event feed for the Logs page (#158),
	// beside the desktop shell's notes. nil renders no sync section. Wired
	// via SetSyncNotes.
	syncNotes func() []devsync.Note
	// setupDetector overrides the /setup source detector (SPEC-0013); nil uses a
	// real HOME-rooted setup.NewDetector(). Tests inject a faked HOME; the desktop
	// layer (#134) injects the genuine macOS Keychain check.
	setupDetector *setup.Detector
	// enabler runs the privileged /setup/enable export→import job (SPEC-0013). It
	// is the seam wired by SetEnabler: the desktop shell backs it with the bundled
	// toolchain, `msgbrowse serve` with a $PATH/config resolver. nil disables
	// Enable (the Setup cards render an "unavailable" affordance and the POST
	// reports it) — the web layer never imports the cgo desktop module.
	enabler Enabler
	// setupTokens is the live per-session token set minted at /setup render and
	// verified on the privileged Setup POSTs (SPEC-0013 §Security same-origin +
	// per-session token). Always non-nil after NewServer.
	setupTokens *setupTokens
	// desktopChrome marks pages as rendered inside the desktop shell's
	// hidden-title-bar window (SPEC-0010 "Native shell affordances", issue
	// #165): page_start adds the `desktop-chrome` <body> class (traffic-light
	// inset padding on the toolbar) and includes /static/desktop.js (the
	// CSP-safe --wails-draggable reader). Set by the embedded server via
	// SetDesktopChrome before serving; browser mode never sets it.
	desktopChrome bool
	// shellNotes supplies the desktop shell's startup diagnostics (systray
	// registration, dock policy — issue #167) for the Logs page, so a tray
	// that fails to render on real hardware is observable instead of silent.
	// nil (browser mode) renders no shell section. Wired via SetShellNotes.
	shellNotes func() []ShellNote
	// externalOpener hands a validated external http(s) URL to the OS default
	// browser — the desktop shell's answer to target="_blank" links, which its
	// webview otherwise drops silently (no new-window handler; issue #179).
	// Wired via SetExternalOpener by the shell only; nil (browser mode) leaves
	// POST /desktop/open-url answering 404.
	externalOpener func(url string) error
	// autostart is the desktop shell's launch-at-login registration (issue
	// #430). Wired via SetAutostart by the shell only; nil (browser mode)
	// renders no toggle and leaves POST /settings/autostart unregistered.
	autostart Autostarter
	// llmConfig is the live LLM settings source behind the Settings → LLM tab
	// (#191): serve and the desktop shell wire an llm.Applier over the shared
	// llm.Holder via SetLLMConfig. nil renders the tab from llmBoot and makes
	// the save POST report itself unavailable.
	llmConfig LLMConfigurator
	// llmBoot is the boot-time LLM config snapshot (file + defaults merged),
	// the tab's display fallback when no configurator is wired.
	llmBoot llm.Settings
	// indexer runs the semantic-index embedding job behind the Status page's
	// Build / Reset-&-rebuild controls (#191): serve and the desktop shell wire
	// an internal/embed.Indexer over the shared store + llm.Holder via
	// SetIndexer. nil (browser / no-op mode) renders the controls' "unavailable"
	// state and makes the Build POST report itself so.
	indexer Indexer
	// indexMu guards indexing, the single-flight flag for the ONE global index
	// job the web layer runs at a time. A second Build while a job is in flight
	// coalesces to a no-op rather than starting a duplicate SQLite writer.
	indexMu  sync.Mutex
	indexing bool

	// journalBuilder runs the journal pass behind the Journal page's Build /
	// Rebuild controls (#240); nil (browser / no-op mode) renders the
	// unavailable state and no controls at all.
	journalBuilder JournalBuilder
	// journalMu guards journalRunning, the single-flight flag for the ONE
	// journal job the web layer runs at a time. Digests are billable outbound
	// calls, so a raced double-start costs money, not just duplicated work.
	journalMu      sync.Mutex
	journalRunning bool

	// factsExtractor runs the contact-fact extraction pass behind the Settings →
	// Facts tab's Extract / Re-extract controls (#366); nil (browser / no-op mode)
	// renders the unavailable state and no controls at all.
	factsExtractor FactsExtractor
	// factsMu guards factsRunning, the single-flight flag for the ONE extraction
	// job the web layer runs at a time. Extraction is billable outbound LLM work
	// over every eligible conversation, so a raced double-start costs real
	// money, not just duplicated work.
	factsMu      sync.Mutex
	factsRunning bool

	// sentimentScorer runs the IPIP scoring pass behind the Settings → Sentiment
	// tab's Score / Rescore controls (#367); nil (browser / no-op mode) renders
	// the unavailable state and no controls at all.
	sentimentScorer SentimentScorer
	// sentimentMu guards sentimentRunning, the single-flight flag for the ONE
	// scoring job the web layer runs at a time. Scoring is the archive's MOST
	// expensive pass — one llm.Chat call per batch over every eligible
	// conversation (ADR-0028) — so a raced double-start costs the most real money
	// of any pipeline here, not just duplicated work.
	sentimentMu      sync.Mutex
	sentimentRunning bool
	// addressBook is the pluggable address-book provider behind contact
	// merging (issue #9): the macOS desktop shell wires a Contacts-backed
	// contacts.Resolver via SetContactResolver; nil (Linux, browser mode,
	// unwired shell) reads as contacts.Unavailable{} through
	// contactResolver(), so consumers never nil-check and the merge path
	// never errors for "no address book".
	addressBook contacts.Resolver
	// journalExclude mirrors journal.exclude_conversations so the /journal stat
	// tiles (most-active weekday, peak hour) honor the same denylist the
	// mechanical journal_days was built with — otherwise the message-scanning
	// stats would leak an excluded conversation's activity (ADR-0023).
	journalExclude []string

	// backupMgr runs the Create / Prune / Restore snapshot operations behind
	// the Backups tab (ADR-0026 / SPEC-0026). nil (browser mode, no data_dir)
	// renders the tab's unavailable state and makes the mutating POSTs report
	// themselves so.
	backupMgr      BackupManager
	backupMu       sync.Mutex
	backupInFlight bool
	// backupConfig is the live backup settings source behind the Backups
	// tab's configuration form (issue #300): serve and the desktop shell wire
	// an applier over the loaded config file via SetBackupConfig. nil renders
	// the form from backupBoot and makes the save POST report itself
	// unavailable.
	backupConfig BackupConfigurator
	// backupBoot is the boot-time backups config snapshot (file + defaults
	// merged), the configuration form's display fallback when no
	// configurator is wired.
	backupBoot config.BackupsConfig
}

// NewServer constructs a Server, parsing templates and wiring routes.
func NewServer(st Store, cfg *config.Config, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		store: st,
		// The root-resolution inputs (issue #160): a value snapshot so a caller
		// mutating cfg after NewServer cannot skew later resolutions. Effective
		// roots are computed per call via archiveRoots(); explicit cfg roots are
		// kept separately for the Enabled signal.
		rootsCfg: config.Config{
			ArchiveRoot:         cfg.ArchiveRoot,
			IMessageArchiveRoot: cfg.IMessageArchiveRoot,
			WhatsAppArchiveRoot: cfg.WhatsAppArchiveRoot,
			DataDir:             cfg.DataDir,
		},
		cfgRoots: archivepath.Roots{
			Signal:   cfg.ArchiveRoot,
			IMessage: cfg.IMessageArchiveRoot,
			WhatsApp: cfg.WhatsAppArchiveRoot,
		},
		derivedDir:        imageconv.DerivedDir(cfg.DataDir),
		log:               log,
		deviceSyncEnabled: cfg.DeviceSync.Enabled,
		setupTokens:       newSetupTokens(),
		journalExclude:    cfg.Journal.ExcludeConversations,
		llmBoot: llm.Settings{
			BaseURL:    cfg.LLM.BaseURL,
			EmbedModel: cfg.LLM.EmbedModel,
			ChatModel:  cfg.LLM.ChatModel,
			APIKey:     cfg.LLM.APIKey,
		},
		backupBoot: cfg.Backups,
	}
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"renderBody":       renderBody,
		"mediaURL":         mediaURL,
		"humanSize":        humanSize,
		"num":              num,
		"duration":         formatDurationMS,
		"domainOf":         domainOf,
		"highlightSnippet": highlightSnippet,
		"humanName":        humanName,
		"reactionTitle":    reactionTitle,
		"initials":         initials,
		"avatarColor":      avatarColor,
		"dateKey":          dateKey,
		"clockTime":        clockTime,
		"dateLabel":        dateLabel,
		"sourceSlug":       sourceSlug,
		"humanSource":      source.Label,
		"summarizeError":   SummarizeEmbedError,
		"imgRenderable":    s.imgRenderable,
		"imgTileState":     s.imgTileState,
		"convRowCtx":       convRowCtx,
		"galleryConvURL":   galleryConvURL,
		// dict returns a map from key/value pairs, for passing multiple
		// arguments to a {{template}} partial call (#227).
		"dict": func(values ...any) map[string]any {
			m := make(map[string]any, len(values)/2)
			for i := 0; i+1 < len(values); i += 2 {
				key, ok := values[i].(string)
				if !ok {
					continue
				}
				m[key] = values[i+1]
			}
			return m
		},
		// safe marks a string as trusted HTML for template output. It is only
		// ever called with literal template text (banner bodies that contain
		// <a>/<code> tags), never with request-derived input (#227).
		"safe": func(s string) template.HTML { return template.HTML(s) },
		"has": func(slice []string, s string) bool {
			for _, v := range slice {
				if v == s {
					return true
				}
			}
			return false
		},
	}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	s.tmpl = tmpl
	tags, err := staticETags(staticFS)
	if err != nil {
		return nil, fmt.Errorf("compute static etags: %w", err)
	}
	s.staticTags = tags
	s.mux = s.routes()
	return s, nil
}

// archiveRoots resolves the effective per-source archive roots for this
// request: the configured cfg root when set, else the managed root iff it
// exists on disk (setup.EffectiveRoots — issue #160). Resolution is per-call
// rather than cached at construction because the FIRST in-session Enable
// creates the managed root AFTER NewServer ran — media must resolve without a
// relaunch. Cost: at most three os.Stat calls (zero for a source with a cfg
// root), microseconds against the SPEC-0008 millisecond budgets.
func (s *Server) archiveRoots() archivepath.Roots {
	return setup.EffectiveRoots(&s.rootsCfg)
}

// Image-tile states for the media grid and the transcript thumbnails
// (issue #15). The strings are what imgTileState hands to the templates.
const (
	tileImg       = "img"       // will actually display in an <img>
	tileNoPreview = "nopreview" // on disk but not browser-renderable: download placeholder
	tileMissing   = "missing"   // absent from the archive on this machine (or unresolvable)
)

// imgTileState classifies an image attachment for rendering (issue #15):
//
//   - tileImg: the <img> src will succeed — a web-native format whose file is
//     on disk, or a non-web format (HEIC/TIFF) whose transcoded JPEG
//     derivative is on disk (the media handler serves the derivative).
//   - tileNoPreview: the original is on disk but no browser-renderable
//     rendition exists (e.g. an un-transcoded HEIC) — templates render a
//     download placeholder.
//   - tileMissing: the attachment row exists in the DB but the file can't be
//     resolved (source not configured, traversal) or isn't on disk (partial
//     export, moved archive, synced replica without media). Templates render
//     an inert labeled placeholder — an <img> would 404 into the browser's
//     broken-image glyph.
//
// The existence check is a stat per tile (plus one for the derivative on
// convertible formats). That is the same per-item cost decorateFiles already
// pays on the Files tab, and microseconds against the SPEC-0008 render
// budgets — the alternative is emitting <img> tags that fail at request time.
func (s *Server) imgTileState(src, convName, relPath string) string {
	abs, ok := s.mediaFilePath(src, convName, relPath)
	if imageconv.Convertible(relPath) {
		// Non-web format: renderable only through its transcoded derivative.
		if !ok {
			return tileMissing
		}
		if d := imageconv.DerivedPath(s.derivedDir, abs); d != "" && statIsFile(d) {
			return tileImg
		}
		if statIsFile(abs) {
			return tileNoPreview
		}
		return tileMissing
	}
	if !ok || !statIsFile(abs) {
		return tileMissing
	}
	if imageconv.WebRenderable(relPath) {
		return tileImg
	}
	return tileNoPreview
}

// imgRenderable reports whether an image attachment will actually display in an
// <img>. Kept as the boolean the transcript templates branch on; the gallery
// uses the finer-grained imgTileState.
func (s *Server) imgRenderable(src, convName, relPath string) bool {
	return s.imgTileState(src, convName, relPath) == tileImg
}

// statIsFile reports whether path exists and is a regular-ish file (not a
// directory) — the same predicate handleMedia applies before serving.
func statIsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Handler returns the root http.Handler (security headers already applied).
func (s *Server) Handler() http.Handler { return s.mux }

// SetDesktopChrome marks rendered pages as living inside the desktop shell's
// hidden-title-bar window (issue #165). The full-page shell then carries the
// `desktop-chrome` <body> class — which pads the unified toolbar past the
// macOS traffic lights and scopes the drag-region script — and loads
// /static/desktop.js. Call before serving; the flag is read-only afterwards.
// This is the minimal template-flag mechanism SPEC-0010 needs: no query
// params, no inline styles, no per-request state — the embedded server knows
// it is the desktop shell at construction time and says so once.
func (s *Server) SetDesktopChrome(enabled bool) { s.desktopChrome = enabled }

// SetDeviceSyncFeature records whether the device-sync feature is compiled into
// this binary (the `devicesync` build tag). The shell passes a build-tagged
// constant; with the feature absent — the default release build — the
// /settings and /status device-sync sections do not render at all, so an
// unfinished feature never ships a visible-but-dead surface. Call before
// serving; the flag is read-only afterwards.
func (s *Server) SetDeviceSyncFeature(enabled bool) { s.deviceSyncFeature = enabled }

// SetShellNotes wires the desktop shell's diagnostics snapshot (issue #167:
// systray/dock startup must be observable on the Logs page, not silent).
// fn is called per /logs render and must be safe for concurrent use; the
// returned notes are server-owned strings (never request- or message-derived),
// rendered through html/template like everything else. Call before serving.
func (s *Server) SetShellNotes(fn func() []ShellNote) { s.shellNotes = fn }

// routes builds the mux and wraps it with the middleware stack: gzip outermost
// (SPEC-0008 REQ-0008-007), then the security headers, then the mux. The order
// is safe because securityHeaders only sets response headers before delegating
// — the gzip wrapper still sees every body write and the headers land on the
// same header map either way.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheStatic(etagStatic(s.staticTags, http.FileServer(http.FS(staticSub))))))

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /search/results", s.handleSearchResults)
	mux.HandleFunc("GET /gallery", s.handleGallery)
	// /media is the header Media tab's URL (#190): the same gallery render at a
	// name matching the tab. /gallery stays the canonical surface — its tab and
	// filter links keep their /gallery?... URLs — and the exact-path pattern
	// never shadows the attachment route below ("GET /media/{id}/{path...}").
	mux.HandleFunc("GET /media", s.handleGallery)
	mux.HandleFunc("GET /gallery/items", s.handleGalleryItems)
	// The editorialized journal (ADR-0023): a mood-tinted month calendar + an
	// editorial day card. Navigation is by query params (?year&month&day), all
	// boosted — no separate continuation route.
	mux.HandleFunc("GET /journal", s.handleJournal)
	// In-app journal building (#240): privileged POSTs behind the same
	// checkSetupPOST gate as every other mutating endpoint. Build fills the
	// mechanical layer plus missing digests; Rebuild all regenerates from
	// scratch; Rebuild day redoes exactly one day. All three start a detached
	// single-flight job and re-render Journal with a fixed-enum banner.
	mux.HandleFunc("POST /journal/build", s.handleJournalBuild)
	mux.HandleFunc("POST /journal/rebuild", s.handleJournalRebuildAll)
	mux.HandleFunc("POST /journal/rebuild/day", s.handleJournalRebuildDay)
	// Live progress fragment: the build card polls this while a run is in
	// flight and stops when the fresh HTML drops the trigger.
	mux.HandleFunc("GET /journal/build/progress", s.handleJournalBuildProgress)
	// In-app fact extraction (#366): privileged POSTs behind the same
	// checkSetupPOST gate. Extract picks up where each conversation's stored
	// cursor stopped; Re-extract clears every fact and cursor first. Both start
	// a detached single-flight job and re-render the Facts tab with a
	// fixed-enum banner, and NEITHER takes any user-supplied scope — the reset
	// flag is chosen by the route, so a hand-crafted POST cannot widen the job.
	mux.HandleFunc("POST /facts/run", s.handleFactsRun)
	mux.HandleFunc("POST /facts/reset", s.handleFactsReset)
	// Live progress fragment: the card polls this while a run is in flight and
	// stops when the fresh HTML drops the trigger.
	mux.HandleFunc("GET /facts/run/progress", s.handleFactsProgress)
	// In-app IPIP sentiment scoring (#367): privileged POSTs behind the same
	// checkSetupPOST gate. Score picks up where each conversation's stored cursor
	// stopped; Rescore clears every score and cursor first (never the opt-outs).
	// Both start a detached single-flight job and re-render the Sentiment tab
	// with a fixed-enum banner, and NEITHER takes any user-supplied scope — the
	// reset flag is chosen by the route, so a hand-crafted POST cannot widen the
	// job. Scoring is billable outbound LLM work (ADR-0028), which is why the
	// control states the cost before the click.
	mux.HandleFunc("POST /sentiment/run", s.handleSentimentRun)
	mux.HandleFunc("POST /sentiment/reset", s.handleSentimentReset)
	// Live progress fragment: the card polls this while a run is in flight and
	// stops when the fresh HTML drops the trigger.
	mux.HandleFunc("GET /sentiment/run/progress", s.handleSentimentProgress)
	mux.HandleFunc("GET /c/{id}", s.handleConversation)
	// Per-person Contact + AI Facts + Profile page (redesign Phase 1), keyed by
	// contact id (the merged-person grain), reached from the transcript header.
	mux.HandleFunc("GET /contact/{id}", s.handleContact)
	mux.HandleFunc("POST /c/{id}/pin", s.handlePin)
	mux.HandleFunc("GET /c/{id}/messages", s.handleMessages)
	mux.HandleFunc("GET /c/{id}/at/{mid}", s.handleConversationAt)
	mux.HandleFunc("GET /status", s.handleStatus)
	// Semantic-index controls (#191): privileged POSTs behind the same
	// checkSetupPOST gate as the Setup POSTs — Build embeds the missing delta,
	// Reset & rebuild clears the index and re-embeds from scratch. Both start a
	// detached single-flight job and re-render Status with a fixed-enum banner.
	mux.HandleFunc("POST /status/index", s.handleStatusIndex)
	mux.HandleFunc("POST /status/index/reset", s.handleStatusIndexReset)
	mux.HandleFunc("GET /status/index/progress", s.handleStatusIndexProgress)
	// The Backups tab (ADR-0026 / SPEC-0026): msgbrowse-owned snapshots
	// (Create / Prune / Restore) plus the read-only external .snapshots
	// inventory. The mutating POSTs follow the Status page's
	// checkSetupPOST gate.
	mux.HandleFunc("GET /backups", s.handleBackups)
	mux.HandleFunc("POST /backups/create", s.handleBackupsCreate)
	mux.HandleFunc("POST /backups/prune", s.handleBackupsPrune)
	mux.HandleFunc("POST /backups/restore", s.handleBackupsRestore)
	mux.HandleFunc("POST /backups/config", s.handleBackupsConfigSave)
	// The Setup surface is presented to the user as "Providers" (its route is
	// /providers); /setup 301-redirects for compatibility with any existing links
	// or bookmarks. The privileged POSTs keep the /setup/* prefix — they are
	// server-internal endpoints the rendered controls target, not user-facing URLs.
	mux.HandleFunc("GET /providers", s.handleSetup)
	mux.HandleFunc("GET /setup", redirectTo("/providers"))
	// The Logs viewer (issue #151): a safe GET diagnostic surface reached from
	// Settings; no mutation, no token.
	mux.HandleFunc("GET /logs", s.handleLogs)
	// Privileged Setup POSTs (SPEC-0013 §Security): each is gated inside its
	// handler by the same-origin + per-session-token check before any work.
	mux.HandleFunc("POST /setup/enable", s.handleSetupEnable)
	mux.HandleFunc("POST /setup/refresh", s.handleSetupRefresh)
	mux.HandleFunc("POST /setup/cancel", s.handleSetupCancel)
	mux.HandleFunc("POST /setup/recheck", s.handleSetupRecheck)
	mux.HandleFunc("POST /setup/disable", s.handleSetupDisable)
	mux.HandleFunc("GET /setup/status/{source}", s.handleSetupStatus)
	// The MCP connection page (#223): the former "Settings" tab is now "MCP",
	// served from /settings/mcp. The old /settings URL redirects to /providers
	// (the new landing tab), following the /setup → /providers precedent.
	mux.HandleFunc("GET /settings/mcp", s.handleSettings)
	mux.HandleFunc("GET /settings", redirectTo("/providers"))
	// The MCP → LLM tab (#191): a safe GET render plus the privileged
	// save POST, gated inside its handler by the same checkSetupPOST contract
	// as every other privileged POST.
	mux.HandleFunc("GET /settings/llm", s.handleSettingsLLM)
	mux.HandleFunc("POST /settings/llm", s.handleSettingsLLMSave)
	mux.HandleFunc("POST /settings/llm/test", s.handleSettingsLLMTest)
	mux.HandleFunc("POST /settings/llm/models", s.handleSettingsLLMModels)
	// The Settings → Contacts tab (#12): the merge-rules settings, de-dup
	// candidate review, and the manual merge/split overrides. A safe GET render
	// plus three privileged POSTs (save rules / merge / split), each gated by
	// the same checkSetupPOST contract and re-rendering the tab with a
	// fixed-enum result banner.
	mux.HandleFunc("GET /settings/search-index", s.handleSettingsSearchIndex)
	mux.HandleFunc("GET /settings/journal", s.handleSettingsJournal)
	// The Settings → Facts tab (#366): the contact-fact extraction pipeline's
	// own tab, one per pipeline like Search index and Journal (SPEC-0004
	// REQ-0004-010). The pipeline's POSTs keep their own /facts/* paths — the
	// #368 precedent, where /journal/build and /status/index kept theirs and
	// only the surface they re-render moved.
	mux.HandleFunc("GET /settings/facts", s.handleSettingsFacts)
	// The Settings → Sentiment tab (#367): the IPIP scoring pipeline's own tab,
	// one per pipeline like the other three (SPEC-0004 REQ-0004-010). Before it,
	// scoring had no in-app surface at all and nothing in internal/web read a
	// single score row.
	mux.HandleFunc("GET /settings/sentiment", s.handleSettingsSentiment)
	mux.HandleFunc("GET /settings/contacts", s.handleSettingsContacts)
	mux.HandleFunc("POST /settings/contacts/rules", s.handleSettingsMergeRules)
	mux.HandleFunc("POST /settings/contacts/merge", s.handleSettingsMerge)
	mux.HandleFunc("POST /settings/contacts/split", s.handleSettingsSplit)
	// Device pairing + unpairing (SPEC-0014 Authentication table): privileged
	// POSTs behind the same checkSetupPOST gate as the Setup POSTs (#157/#158).
	mux.HandleFunc("POST /settings/devices/pair", s.handleDevicePair)
	mux.HandleFunc("POST /settings/devices/unpair", s.handleDeviceUnpair)
	mux.HandleFunc("GET /media/{id}/{path...}", s.handleMedia)
	// Desktop-only external-link bridge (issue #179): registered only when the
	// shell wired an opener, so in plain `msgbrowse serve` the route does not
	// exist at all — 404 on every method, never a 405 advertising a POST
	// surface browser mode doesn't have. SetExternalOpener rebuilds the mux to
	// pick this up (wiring precedes serving). The handler still gates on the
	// desktop-chrome flag and the Setup POSTs' same-origin rigor inside.
	if s.externalOpener != nil {
		mux.HandleFunc("POST /desktop/open-url", s.handleOpenURL)
	}
	// Launch-at-login toggle (issue #430): same registration-only-when-wired
	// contract as the open-url bridge above — browser mode never sees the
	// route, and the settings page never sees a dead toggle.
	if s.autostart != nil {
		mux.HandleFunc("POST /settings/autostart", s.handleAutostart)
	}

	// Unknown routes render the styled error page (audit F7, 2026-09-05)
	// instead of net/http's bare "404 page not found" text. Wrapping rather
	// than registering a "/" catch-all: a catch-all also swallows the mux's
	// 405 Method Not Allowed responses, which the security tests pin. Only
	// unmatched requests (empty pattern) pay the probe.
	return gzipMiddleware(securityHeaders(s.styledNotFound(mux)))
}

// styledNotFound serves mux but replaces net/http's bare-text 404 for unknown
// routes with the styled error page. Method-mismatch 405s keep their stdlib
// shape — the mux deliberately advertises registered POST surfaces.
func (s *Server) styledNotFound(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, pat := mux.Handler(r); pat == "" {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code == http.StatusNotFound {
				s.notFound(w, r)
				return
			}
			if rec.Code == http.StatusMethodNotAllowed {
				if allow := rec.Header().Get("Allow"); allow != "" {
					w.Header().Set("Allow", allow)
				}
				http.Error(w, rec.Body.String(), rec.Code)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// Run starts the HTTP server on addr and blocks until ctx is cancelled, then
// shuts down gracefully. addr should normally be loopback (127.0.0.1:8787).
// It is Listen followed by Serve; callers that need the bound address before
// serving — the desktop shell binds 127.0.0.1:0 and reads the ephemeral port
// off the listener (SPEC-0010 "Embedded server on a loopback ephemeral port")
// — call the two halves directly.
func (s *Server) Run(ctx context.Context, addr string) error {
	ln, err := s.Listen(addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Listen opens the TCP listener for addr and logs where the UI is reachable.
// Passing a ":0" port yields an ephemeral port; the caller discovers it from
// the returned listener's Addr.
func (s *Server) Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	bound := ln.Addr().String()
	if !isLoopback(bound) {
		s.log.Warn("listening on a non-loopback address; the UI has no authentication", "addr", bound)
	}
	s.log.Info("web UI listening", "addr", "http://"+bound)
	return ln, nil
}

// Serve serves HTTP on ln and blocks until ctx is cancelled, then shuts down
// gracefully, draining in-flight requests. This is the single shutdown code
// path shared by `msgbrowse serve` (whose context is cancelled by
// SIGINT/SIGTERM) and the desktop shell (whose context is cancelled when the
// window closes) — SPEC-0010 "Graceful shutdown". Serve closes ln on return.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return s.ServeHandler(ctx, ln, s.mux)
}

// ServeHandler is Serve with root handler h in place of the server's own mux.
// The desktop shell uses it to mount the MCP streamable-HTTP handler beside
// the web app on the one embedded loopback listener — SPEC-0010's bind
// surface allows no listener beyond the embedded server — while every web
// route still flows through s.Handler() unchanged. Timeouts and the graceful
// shutdown path are identical to Serve's.
func (s *Server) ServeHandler(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// securityHeaders applies a strict CSP and related hardening to every response.
// The CSP allows only same-origin scripts/styles/images (plus data: images for
// inline placeholders) and forbids framing — message content cannot load or run
// external resources.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'none'; " +
		"script-src 'self'; " +
		"style-src 'self'; " +
		"img-src 'self' data:; " +
		"connect-src 'self'; " +
		"font-src 'self'; " +
		"base-uri 'none'; " +
		"form-action 'self'; " +
		"frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// cacheStatic adds a modest cache lifetime to embedded static assets.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// staticETags computes a strong, content-derived ETag for every embedded
// static asset at startup (SPEC-0008 REQ-0008-008). Embedded files have zero
// modtimes, so http.FileServer can never revalidate by time — a sha256 prefix
// of the bytes gives clients a stable validator instead. Keys are the paths as
// requested under /static/ (e.g. "app.css").
func staticETags(fsys fs.FS) (map[string]string, error) {
	tags := make(map[string]string)
	err := fs.WalkDir(fsys, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(b)
		tags[strings.TrimPrefix(p, "static/")] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// etagStatic serves 304 Not Modified for embedded statics the client already
// holds (If-None-Match against the startup-computed ETag) and stamps the ETag
// on full responses so future revisits can revalidate (REQ-0008-008). Unknown
// paths fall through untouched — the file server 404s as before.
func etagStatic(tags map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// StripPrefix left a path like "app.css"; normalize defensively.
		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if tag, ok := tags[p]; ok {
			w.Header().Set("ETag", tag)
			if etagMatch(r.Header.Get("If-None-Match"), tag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// etagMatch reports whether an If-None-Match header value matches the given
// entity tag. GET revalidation uses weak comparison, so a W/ prefix on the
// client's tag is ignored; "*" matches anything.
func etagMatch(header, tag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == "*" || candidate == tag {
			return true
		}
	}
	return false
}

// redirectTo returns a handler that permanently redirects to target. It backs the
// /setup → /providers compatibility redirect (the page was renamed "Providers"):
// 301 so browsers and htmx boosted navigations both follow it transparently and
// caches learn the canonical URL. target is a fixed app-owned path, never
// request-derived.
func redirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	}
}

// isLoopback reports whether addr's host is a loopback address.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" || host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
