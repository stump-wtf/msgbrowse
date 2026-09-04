// Package config defines msgbrowse's configuration model and the Viper binding
// that loads it from (in increasing order of precedence) built-in defaults, a
// YAML config file, MSGBROWSE_* environment variables, and command-line flags.
//
// Secrets (notably the LLM API key) may be persisted to the 0600 config file by
// the Settings → LLM tab (SaveLLM); the MSGBROWSE_LLM_API_KEY environment
// variable always takes precedence at startup. The config file must never be
// committed. See SECURITY.md for the egress and secret-handling model.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config is the fully-resolved runtime configuration for every msgbrowse
// subcommand. Field tags map each key to its Viper/YAML name.
type Config struct {
	// ArchiveRoot is the path to the signal-export archive. It is treated as
	// strictly read-only; msgbrowse never writes inside it.
	ArchiveRoot string `mapstructure:"archive_root"`

	// IMessageArchiveRoot is the path to the imessage-exporter output (a flat
	// directory of <ChatName>.txt files plus an attachments/ folder). Read-only,
	// like ArchiveRoot. Empty when iMessage import is not used.
	IMessageArchiveRoot string `mapstructure:"imessage_archive_root"`

	// WhatsAppArchiveRoot is the path to the WhatsApp-Chat-Exporter output
	// directory (result.json plus the media folders the tool copied). Read-only,
	// like the other roots. Empty when WhatsApp import is not used (SPEC-0009
	// REQ-0009-001).
	WhatsAppArchiveRoot string `mapstructure:"whatsapp_archive_root"`

	// DataDir is a writable directory (outside the archive) for the SQLite
	// database, vector index, and caches.
	DataDir string `mapstructure:"data_dir"`

	// SignalExportBin / IMessageExporterBin / WhatsAppExporterBin are optional
	// explicit paths to the upstream exporters, mirroring the `msgbrowse export`
	// --*-bin flags. Empty (the default) means "look the default console name up
	// on $PATH". They back the Setup Enable flow's PATH resolver (SPEC-0013) and
	// `export`'s bin overrides from one config source.
	SignalExportBin     string `mapstructure:"signal_export_bin"`
	IMessageExporterBin string `mapstructure:"imessage_exporter_bin"`
	WhatsAppExporterBin string `mapstructure:"whatsapp_exporter_bin"`

	// ListenAddr is the web UI bind address. It defaults to loopback; binding to
	// a non-loopback interface is an explicit, deliberate choice.
	ListenAddr string `mapstructure:"listen_addr"`

	// LLM configures the single OpenAI-compatible provider used for embeddings,
	// RAG synthesis, and journal digests.
	LLM LLMConfig `mapstructure:"llm"`

	// Providers configures the in-app message-source ("Providers") surface,
	// including how often Enabled sources auto-refresh.
	Providers ProvidersConfig `mapstructure:"providers"`

	// VectorBackend selects the vector store: "sqlite-vec" (default) or "qdrant".
	VectorBackend string `mapstructure:"vector_backend"`

	// Journal configures journal generation and the LLM digest pass.
	Journal JournalConfig `mapstructure:"journal"`

	// Backups configures msgbrowse-owned snapshots of data_dir (the SQLite
	// database + embeddings) and the config file (ADR-0026 / SPEC-0026).
	// A snapshot is a plaintext copy of the entire message corpus, created
	// from the Backups tab or `msgbrowse backups create`, listed, pruned by
	// GFS policy, and restorable. Defaults: dir <data_dir>/backups,
	// retention 14/12/4/2.
	Backups BackupsConfig `mapstructure:"backups"`

	// Spam configures the unsolicited-contact evidence layer (ADR-0029).
	// Every key is inert until `msgbrowse spam scan` is run: the scan is a
	// deliberate command, never a side effect of import or serving, and it
	// performs no network egress at all.
	Spam SpamConfig `mapstructure:"spam"`

	// IngestOnStart triggers an ingest pass when `serve` boots.
	IngestOnStart bool `mapstructure:"ingest_on_start"`

	// Watch enables the fsnotify watcher inside `serve` (equivalent to running
	// `msgbrowse watch` alongside the server).
	Watch bool `mapstructure:"watch"`

	// DeviceSync configures multi-device archive synchronization (ADR-0021).
	// Disabled by default: with the block absent, no sync listener exists and
	// the loopback-only posture (ADR-0010) is unchanged.
	DeviceSync DeviceSyncConfig `mapstructure:"device_sync"`

	// LogLevel is one of debug, info, warn, error.
	LogLevel string `mapstructure:"log_level"`

	// SourceFile is the path of the YAML config file this configuration was
	// loaded from, or "" when no config file was found (defaults + env only).
	// It is not a config key (mapstructure:"-"): Unmarshal records it from the
	// Viper instance so the Settings → LLM save handler (#191) can write back
	// to the exact file the process actually loaded.
	SourceFile string `mapstructure:"-"`
}

// DeviceSyncConfig configures device pairing and archive sync (ADR-0021 /
// SPEC-0014). The block is named device_sync — the `sync` word alone belongs
// to ADR-0015's export→import pipeline; every device-sync surface uses the
// `devices` namespace (internal/devices, `msgbrowse devices …`), with this
// config key as the one spelled-out exception for readability.
//
// The SPEC-0011 keys this block used to carry (poll_interval, staging_dir)
// were retired with the bespoke transport (#158): Syncthing owns transfer
// resumption and convergence, so there is no replica polling loop and no
// staging area to configure.
//
// Governing: ADR-0021, SPEC-0014 REQ "Supervised Daemon Lifecycle" — disabled
// by default, dedicated P2P port distinct from the web UI, web UI bind
// unchanged.
type DeviceSyncConfig struct {
	// Enabled turns device sync on. False (the default) means no Syncthing
	// process, no P2P listener, and inert sync-state tables.
	Enabled bool `mapstructure:"enabled"`

	// ListenAddr is the Syncthing P2P sync listener bind address (host:port).
	// Unlike the web UI it is expected to bind a LAN interface; it must use a
	// port distinct from listen_addr.
	ListenAddr string `mapstructure:"listen_addr"`

	// DeviceName is this node's human-readable name, shown on paired peers.
	// Empty means "derive from the hostname" at enablement time.
	DeviceName string `mapstructure:"device_name"`

	// SyncthingBin is an optional explicit path to the Syncthing binary the
	// supervisor runs (ADR-0021: Syncthing is the device-sync transfer
	// engine). Empty (the default) means "look `syncthing` up on $PATH" —
	// the bring-your-own fallback for `msgbrowse serve`, mirroring the
	// exporter *_bin keys above. The desktop .app ignores this key and
	// always resolves its bundled, version-pinned binary from
	// Contents/Resources (never $PATH), per SPEC-0014 REQ "Bundled
	// Syncthing Runtime".
	SyncthingBin string `mapstructure:"syncthing_bin"`
}

// ProvidersConfig configures the in-app Providers surface. `serve` and the
// desktop shell run a background scheduler that re-imports each Enabled source's
// delta every RefreshInterval, so the archive stays current without a click.
type ProvidersConfig struct {
	// RefreshInterval is how often Enabled sources auto-refresh (export +
	// incremental import). Zero (or negative) turns auto-refresh off, leaving
	// only the per-source manual Refresh control.
	RefreshInterval time.Duration `mapstructure:"refresh_interval"`
}

// LLMConfig configures the OpenAI-compatible client. BaseURL is the only network
// egress msgbrowse performs; by default it points at a local LiteLLM proxy.
type LLMConfig struct {
	BaseURL        string        `mapstructure:"base_url"`
	APIKey         string        `mapstructure:"api_key"`
	ChatModel      string        `mapstructure:"chat_model"`
	EmbedModel     string        `mapstructure:"embed_model"`
	MaxConcurrency int           `mapstructure:"max_concurrency"`
	Timeout        time.Duration `mapstructure:"timeout"`

	// Retry bounds transient-failure retries (429/502/503/504 and client
	// timeouts) so one bad gateway response no longer aborts a multi-hour
	// embed/journal/sentiment pass (issue #452). Zero fields mean the client
	// defaults: 3 attempts, 2s base delay, 30s ceiling. attempts=1 disables.
	Retry LLMRetryConfig `mapstructure:"retry"`
}

// LLMRetryConfig is the llm.retry block. See LLMConfig.Retry.
type LLMRetryConfig struct {
	Attempts   int           `mapstructure:"attempts"`
	BaseDelay  time.Duration `mapstructure:"base_delay"`
	MaxBackoff time.Duration `mapstructure:"max_backoff"`
}

// JournalConfig configures `msgbrowse journal`.
type JournalConfig struct {
	// DigestEnabled turns the LLM digest pass on or off. The mechanical journal
	// is always written regardless.
	DigestEnabled bool `mapstructure:"digest_enabled"`

	// DigestPrompt is the system/instruction prompt used for the digest pass.
	// Changing it bumps the effective prompt version and invalidates the cache.
	DigestPrompt string `mapstructure:"digest_prompt"`

	// ExcludeConversations is a denylist of conversation folder names whose
	// content is NEVER sent to the LLM (privacy control).
	ExcludeConversations []string `mapstructure:"exclude_conversations"`

	// MaxDaysPerRun caps how many days a single digest run will process.
	MaxDaysPerRun int `mapstructure:"max_days_per_run"`
}

// SpamConfig is the classification policy for the unsolicited-contact evidence
// layer (ADR-0029 / SPEC-0028). Every field participates in the ruleset version
// stamped on each finding, so changing any of them re-derives the whole
// evidence layer on the next scan — findings produced under two different rule
// sets are not comparable and must never share a dossier.
//
// The block is entirely optional. With it absent the scan still runs and still
// records who messaged you and what you sent back; it simply has no watch list,
// no name variants, and no opt-out notice to match, so nothing is flagged as a
// candidate.
type SpamConfig struct {
	// MyNumbers are the archive owner's own identifiers. They are never
	// counterparties.
	MyNumbers []string `mapstructure:"my_numbers"`

	// Allowlist holds identifiers that are never candidates even though no
	// address book knows them: banks, 2FA short codes, delivery notifications.
	Allowlist []string `mapstructure:"allowlist"`

	// WatchAreaCodes are NANP area codes (no country code) worth flagging.
	WatchAreaCodes []string `mapstructure:"watch_area_codes"`

	// NameVariants are the owner's first name and every misspelling a sender
	// has used. A stranger using your name is evidence they believe they know
	// who they are texting.
	NameVariants []string `mapstructure:"name_variants"`

	// FlagAnyURL makes a bare link a reason on its own, not just a shortener.
	FlagAnyURL bool `mapstructure:"flag_any_url"`

	// ShortenerDomains overrides the built-in link-shortener denylist.
	ShortenerDomains []string `mapstructure:"shortener_domains"`

	// EntityKeywords seed the industry guess. They are leads, never findings.
	EntityKeywords []string `mapstructure:"entity_keywords"`

	// StopKeywords are outbound bodies that, standing alone, count as an
	// opt-out.
	StopKeywords []string `mapstructure:"stop_keywords"`

	// CannedNotice is the owner's DNC/TCPA notice, matched as a normalized
	// prefix so autocorrect and a trimmed send still register.
	CannedNotice string `mapstructure:"canned_notice"`

	// CannedNoticeMatchRatio is the fraction of CannedNotice that must appear.
	CannedNoticeMatchRatio float64 `mapstructure:"canned_notice_match_ratio"`

	// ExcludeConversations is a denylist of conversation names the scan never
	// reads. It is separate from journal.exclude_conversations on purpose: that
	// list is about what may be sent to an LLM, this one about what may enter
	// an evidence record, and the right answers differ.
	ExcludeConversations []string `mapstructure:"exclude_conversations"`

	// ExportDir is where `msgbrowse spam evidence` writes dossiers. Empty means
	// <data_dir>/spam-exports. A dossier is a plaintext copy of every message
	// from one sender; the directory is created 0700 and files 0600.
	ExportDir string `mapstructure:"export_dir"`
}

// BackupsConfig configures msgbrowse-owned snapshots (ADR-0026 / SPEC-0026).
// A snapshot is a plaintext copy of data_dir (the SQLite database + embeddings)
// and the config file, created from the Backups tab or `msgbrowse backups
// create`, listed, pruned by GFS policy, and restorable. Snapshot files are
// mode 0600 / directory 0700 — they contain the entire message corpus
// (ADR-0013: no SQLCipher in-process).
type BackupsConfig struct {
	// Dir is the directory for msgbrowse-owned snapshots. Empty (the default)
	// means <data_dir>/backups. MUST NOT be inside archive_root (the read-only
	// archive; ADR-0010 §4) — startup warns and refuses the path if it is.
	Dir string `mapstructure:"dir"`

	// Retention is the GFS tier policy applied by prune. Zero values fall back
	// to the defaults (14 daily / 12 monthly / 4 quarterly / 2 yearly).
	Retention RetentionConfig `mapstructure:"retention"`
}

// RetentionConfig is the per-tier keep count for GFS pruning. A zero value
// means "use the default" so an unset block still gets sane policy.
type RetentionConfig struct {
	Daily     int `mapstructure:"daily"`
	Monthly   int `mapstructure:"monthly"`
	Quarterly int `mapstructure:"quarterly"`
	Yearly    int `mapstructure:"yearly"`
}

// DefaultRetention is the GFS keep-count policy used when no retention block
// is configured. These mirror the tier age boundaries in internal/ingest.
var DefaultRetention = RetentionConfig{
	Daily: 14, Monthly: 12, Quarterly: 4, Yearly: 2,
}

// EffectiveRetention returns the retention config with zero values replaced by
// the defaults, so callers never have to nil-check individual tiers.
func (b BackupsConfig) EffectiveRetention() RetentionConfig {
	r := b.Retention
	if r.Daily == 0 {
		r.Daily = DefaultRetention.Daily
	}
	if r.Monthly == 0 {
		r.Monthly = DefaultRetention.Monthly
	}
	if r.Quarterly == 0 {
		r.Quarterly = DefaultRetention.Quarterly
	}
	if r.Yearly == 0 {
		r.Yearly = DefaultRetention.Yearly
	}
	return r
}

// DefaultDigestPrompt is the built-in journal digest instruction. Its text is
// part of the digest cache key (prompt version), so edits here are intentional
// and automatically re-derive every cached digest on the next run.
//
// It demands a single JSON object (parsed tolerantly in internal/journal,
// mirroring the strict-JSON contract in internal/facts). The mood enum here MUST
// stay in sync with internal/journal.Moods — an unknown mood is coerced to
// "neutral" on parse, so drift degrades gracefully rather than failing.
const DefaultDigestPrompt = "You are summarizing one day of a personal message archive (Signal, iMessage, WhatsApp). " +
	"Return ONLY a single JSON object, no prose, no markdown fences, with exactly these keys: " +
	`"summary" (a 1-3 sentence factual summary, string), ` +
	`"people" (array of strings, the key participants by the names they appear under), ` +
	`"themes" (array of short topic-tag strings), ` +
	`"mood" (one of: upbeat, neutral, quiet, tense), ` +
	`"highlights" (array of objects {"text": string, "time": "HH:MM"} for notable moments, 24-hour time from the transcript), ` +
	`"standout_media" (array of notable attachment filenames), ` +
	`"notable_links" (array of URLs copied verbatim from the transcript). ` +
	"Rules: be factual and neutral; do NOT invent details, names, times, or links not in the transcript. " +
	`If a field has nothing, use an empty array (or "" for summary, "neutral" for mood). ` +
	"Mood reflects the overall tone of the whole day, not any single message."

// SetDefaults registers every default value on the given Viper instance. It is
// the single source of truth for built-in defaults and is also used by tests.
func SetDefaults(v *viper.Viper) {
	v.SetDefault("archive_root", "")
	v.SetDefault("imessage_archive_root", "")
	v.SetDefault("whatsapp_archive_root", "")
	v.SetDefault("data_dir", "./data")

	// Optional overrides for the upstream exporters `msgbrowse export` invokes.
	// Empty means "look up the default name on PATH" (sigexport /
	// imessage-exporter / wtsexporter); set a path here (or via
	// --signal-export-bin / --imessage-exporter-bin / --whatsapp-exporter-bin,
	// or MSGBROWSE_SIGNAL_EXPORT_BIN / MSGBROWSE_IMESSAGE_EXPORTER_BIN /
	// MSGBROWSE_WHATSAPP_EXPORTER_BIN) to use a specific binary (e.g. one in a
	// pipx venv not on PATH).
	v.SetDefault("signal_export_bin", "")
	v.SetDefault("imessage_exporter_bin", "")
	v.SetDefault("whatsapp_exporter_bin", "")
	v.SetDefault("listen_addr", "127.0.0.1:8787")

	v.SetDefault("llm.base_url", "http://127.0.0.1:4000/v1")
	v.SetDefault("llm.api_key", "")
	// Local-first defaults: these are LiteLLM route aliases meant to resolve to a
	// local model (matching the loopback llm.base_url above). Routing to a hosted
	// model must be a deliberate choice — see docs/adr/0010-security-privacy-posture.md.
	v.SetDefault("llm.chat_model", "local-chat")
	v.SetDefault("llm.embed_model", "local-embed")
	v.SetDefault("llm.max_concurrency", 4)
	v.SetDefault("llm.timeout", 60*time.Second)
	// llm.retry zero-values mean the client's own defaults (issue #452); only
	// the ceiling needs an explicit default so a misconfigured base delay
	// cannot sleep unbounded.
	v.SetDefault("llm.retry.max_backoff", 30*time.Second)

	// Providers auto-refresh: re-import each Enabled source's delta on this
	// cadence. 6h keeps archives current without hammering the exporters; set
	// providers.refresh_interval to 0 to disable.
	v.SetDefault("providers.refresh_interval", 6*time.Hour)

	v.SetDefault("vector_backend", "sqlite-vec")

	v.SetDefault("journal.digest_enabled", true)
	v.SetDefault("journal.digest_prompt", DefaultDigestPrompt)
	v.SetDefault("journal.exclude_conversations", []string{})
	v.SetDefault("journal.max_days_per_run", 0) // 0 = unbounded

	// msgbrowse-owned snapshots (ADR-0026 / SPEC-0026): empty dir means
	// <data_dir>/backups; retention defaults to 14/12/4/2.
	v.SetDefault("backups.dir", "")
	v.SetDefault("backups.retention.daily", DefaultRetention.Daily)
	v.SetDefault("backups.retention.monthly", DefaultRetention.Monthly)
	v.SetDefault("backups.retention.quarterly", DefaultRetention.Quarterly)
	v.SetDefault("backups.retention.yearly", DefaultRetention.Yearly)

	v.SetDefault("spam.my_numbers", []string{})
	v.SetDefault("spam.allowlist", []string{})
	v.SetDefault("spam.watch_area_codes", []string{})
	v.SetDefault("spam.name_variants", []string{})
	v.SetDefault("spam.flag_any_url", true)
	v.SetDefault("spam.shortener_domains", []string{})
	v.SetDefault("spam.entity_keywords", []string{})
	v.SetDefault("spam.stop_keywords", []string{})
	v.SetDefault("spam.canned_notice", "")
	v.SetDefault("spam.canned_notice_match_ratio", 0.6)
	v.SetDefault("spam.exclude_conversations", []string{})
	v.SetDefault("spam.export_dir", "")

	v.SetDefault("ingest_on_start", false)
	v.SetDefault("watch", false)
	v.SetDefault("log_level", "info")

	// Device sync (ADR-0018) is strictly opt-in: enabled=false means no
	// listener and no change to the loopback-only posture. The default port
	// is deliberately distinct from the web UI's 8787.
	v.SetDefault("device_sync.enabled", false)
	v.SetDefault("device_sync.listen_addr", ":8788")
	v.SetDefault("device_sync.device_name", "")
	// Optional override for the Syncthing binary the device-sync supervisor
	// runs (ADR-0021). Empty means "look `syncthing` up on PATH" for the
	// bring-your-own CLI path; the desktop .app always uses its bundled copy.
	v.SetDefault("device_sync.syncthing_bin", "")
}

// Load constructs a *viper.Viper wired for msgbrowse: defaults, optional config
// file, and MSGBROWSE_* environment variables. cfgFile may be empty, in which
// case the standard search paths are used. Flags are bound separately by the CLI
// layer via BindPFlags.
func Load(cfgFile string) (*viper.Viper, error) {
	v := viper.New()
	SetDefaults(v)

	v.SetEnvPrefix("MSGBROWSE")
	// Map e.g. MSGBROWSE_LLM_API_KEY -> llm.api_key.
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("$HOME/.config/msgbrowse")
		// The platform user-config location (macOS: ~/Library/Application
		// Support/msgbrowse) — where the desktop app creates the config file
		// when the Settings → LLM tab saves with no file present (#191). On
		// Linux this usually duplicates $HOME/.config/msgbrowse; the duplicate
		// search path is harmless.
		if ucd, err := os.UserConfigDir(); err == nil {
			v.AddConfigPath(filepath.Join(ucd, "msgbrowse"))
		}
		v.AddConfigPath("/etc/msgbrowse")
	}

	if err := v.ReadInConfig(); err != nil {
		// A missing config file is fine; defaults + env + flags still apply.
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("reading config: %w", err)
		}
	}

	return v, nil
}

// Unmarshal materializes a Config from the given Viper instance. It records
// the config file Viper actually read (if any) in SourceFile so save-back
// surfaces target the loaded file, never a guessed one.
func Unmarshal(v *viper.Viper) (*Config, error) {
	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("decoding config: %w", err)
	}
	c.SourceFile = v.ConfigFileUsed()
	return &c, nil
}

// Validate checks the resolved configuration for the invariants every subcommand
// relies on. It does not require the archive to exist for commands that do not
// read it; callers that need the archive should check ArchiveRoot themselves.
func (c *Config) Validate() error {
	switch c.VectorBackend {
	case "sqlite-vec", "qdrant":
	default:
		return fmt.Errorf("invalid vector_backend %q (want sqlite-vec or qdrant)", c.VectorBackend)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log_level %q", c.LogLevel)
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	// ADR-0026: backups.dir MUST NOT be inside archive_root (the read-only
	// archive; ADR-0010 §4). An operator who points it there would surprise
	// a :ro mount and violate the read-only contract.
	if c.Backups.Dir != "" && c.ArchiveRoot != "" {
		absBackups, _ := filepath.Abs(c.Backups.Dir)
		absArchive, _ := filepath.Abs(c.ArchiveRoot)
		if isWithin(absBackups, absArchive) {
			return fmt.Errorf("backups.dir %q is inside archive_root %q — backups must not be written into the read-only archive (ADR-0010 §4)",
				c.Backups.Dir, c.ArchiveRoot)
		}
	}
	if c.DeviceSync.Enabled {
		if c.DeviceSync.ListenAddr == "" {
			return fmt.Errorf("device_sync.listen_addr must not be empty when device_sync.enabled is true")
		}
		// SPEC-0011 "Sync Listener Posture": the sync listener needs a
		// dedicated PORT distinct from the web UI's. Naive string equality
		// misses spellings of the same port ("127.0.0.1:8787" vs ":8787"), so
		// compare the ports themselves (#115 review fold-in).
		syncPort, err := listenPort(c.DeviceSync.ListenAddr)
		if err != nil {
			return fmt.Errorf("invalid device_sync.listen_addr %q: %w", c.DeviceSync.ListenAddr, err)
		}
		webPort, err := listenPort(c.ListenAddr)
		if err != nil {
			return fmt.Errorf("invalid listen_addr %q: %w", c.ListenAddr, err)
		}
		if syncPort == webPort {
			return fmt.Errorf("device_sync.listen_addr %q uses the web UI port %d; the sync listener needs its own port (SPEC-0014)",
				c.DeviceSync.ListenAddr, webPort)
		}
	}
	return nil
}

// listenPort extracts and validates the numeric port of a host:port listen
// address. Port 0 (ephemeral) is allowed — tests and the desktop shell bind
// :0 deliberately — but the string must parse as a port number.
func listenPort(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return 0, fmt.Errorf("port %q is not a number in 0-65535", portStr)
	}
	return port, nil
}

// isWithin reports whether child is the same as or a subdirectory of parent.
// Both paths MUST be absolute and cleaned; the caller is expected to have
// run filepath.Abs on each. It is lexical — it does not resolve symlinks —
// which is correct for the backups.dir-in-archive check: the contract is
// "the configured path must not be under the configured archive root", not
// "a symlink might escape".
func isWithin(child, parent string) bool {
	if parent == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
