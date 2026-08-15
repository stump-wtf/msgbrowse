package cli

import (
	"image/color"

	"charm.land/fang/v2"
	"charm.land/lipgloss/v2"
)

// This file re-skins fang's help/error layout (the "Crush look") with
// msgbrowse's own design language: the Slate daisyUI themes the web UI ships
// (ADR-0012, SPEC-0006). The layout — aligned command/flag tables, usage
// codeblock, ERROR badge — stays fang's; only the colors become ours.
//
// The hexes below mirror internal/web/tailwind/input.css verbatim, and
// helptheme_test.go asserts that mapping against the token sheet so the CLI
// cannot drift from the web theme.

// slatePalette is the subset of one Slate theme's daisyUI tokens the CLI
// scheme is built from.
type slatePalette struct {
	base100     string // --color-base-100: app background
	base200     string // --color-base-200: raised panel (usage codeblock bg)
	baseContent string // --color-base-content: primary text
	primary     string // --color-primary: slate-blue accent
	secondary   string // --color-secondary: light accent
	success     string // --color-success: iMessage green
	warning     string // --color-warning: search-highlight gold
	errColor    string // --color-error
	errContent  string // --color-error-content: text on the error badge
	dim         string // CLI-only, see below
}

// The dim value has no token in the sheet: the web dims via
// color-mix(in oklab, <content> X%, transparent), which cannot be carried into
// a terminal palette. It is therefore derived per theme as base-content mixed
// 60% (dark) / 65% (light) over base-100 — chosen so FlagDefault/Comment hold
// ≥4.5:1 against both the terminal background and the usage codeblock
// (spot-checked dark 5.5:1, light 4.6:1). If the themes are ever redesigned,
// re-derive these alongside the token copy.
var (
	// slateDark carries the dark "slate" theme (input.css, default: true).
	slateDark = slatePalette{
		base100:     "#0f1216",
		base200:     "#13171c",
		baseContent: "#dbe2ea",
		primary:     "#6f93d6",
		secondary:   "#9bb6e6",
		success:     "#34c759",
		warning:     "#e8b84b",
		errColor:    "#f43f5e",
		errContent:  "#0f1216",
		dim:         "#898f95",
	}

	// slateLight carries the "slate-light" theme (input.css).
	slateLight = slatePalette{
		base100:     "#f7f9fc",
		base200:     "#eef2f8",
		baseContent: "#1a2330",
		primary:     "#3f63ad",
		secondary:   "#5b7cc0",
		success:     "#15803d",
		warning:     "#b7791f",
		errColor:    "#be123c",
		errContent:  "#f7f9fc",
		dim:         "#676e77",
	}
)

// Contrast-driven light-mode substitutes. Three slate-light tokens miss the
// 4.5:1 normal-text floor the rest of the mapping holds, measured against both
// surfaces they render on — the terminal background (base-100) and the usage
// codeblock fang paints (base-200):
//
//	secondary #5b7cc0  3.91:1 / 3.67:1  → Command falls back to primary
//	success   #15803d  4.76:1 / 4.46:1  → Flag uses lightFlag
//	warning   #b7791f  3.45:1 / 3.24:1  → QuotedString uses lightQuotedString
//
// success clears the floor on base-100 and misses it only on base-200 — but
// base-200 is the surface fang actually paints behind the usage codeblock,
// where flags are rendered, so it is the one that decides.
//
// The two hexes below are CLI-only derivations of the same hue (like dim
// above), darkened until they clear the floor on both surfaces. The dark theme
// misses nothing and carries its tokens verbatim.
const (
	lightFlag         = "#137535" // success darkened: 5.49:1 / 5.16:1
	lightQuotedString = "#96610a" // warning darkened: 4.96:1 / 4.66:1
)

// colorScheme maps the palette onto fang's roles:
//
//	Title/Program  primary          the slate-blue accent leads
//	Command        secondary        light accent for subcommand names
//	Flag           success          iMessage green for flags
//	QuotedString   warning          the gold family the web reserves for highlights
//	Base/Description/Argument       base-content
//	Codeblock      base-200         the raised-panel surface behind the usage line
//	FlagDefault/Comment/DimmedArgument  dim
//	ErrorHeader    errContent on errColor  the daisyUI error badge pairing
//
// Three of fang.ColorScheme's sixteen fields are deliberately left unset:
// Help, Dash and ErrorDetails have no consumer. fang's makeStyles never reads
// them (verified by grep over fang v2.0.1 — they appear only at their struct
// declaration and in DefaultColorScheme), and neither does slateStyles. Mapping
// them only invited the contrast test to prove a property of three values that
// never reach a terminal (#333, M4).
//
// One guarantee that is easy to misread while looking at this table: the
// ERROR *body* is not governed by it. fang builds styles.ErrorText with no
// Foreground at all — only MarginLeft, Width and (in fang's own makeStyles) the
// title-case transform — so error text renders in whatever foreground colour
// the terminal already has. Only the ERROR badge itself (ErrorHeader) is ours.
//
// Every role that does render holds ≥4.5:1 against base-100 and base-200 —
// helptheme_test.go asserts it — via the light-mode substitutes documented
// above.
func (p slatePalette) colorScheme() fang.ColorScheme {
	base := lipgloss.Color(p.baseContent)
	accent := lipgloss.Color(p.primary)
	command := lipgloss.Color(p.secondary)
	flag := lipgloss.Color(p.success)
	quoted := lipgloss.Color(p.warning)
	if p == slateLight {
		command = accent
		flag = lipgloss.Color(lightFlag)
		quoted = lipgloss.Color(lightQuotedString)
	}
	dim := lipgloss.Color(p.dim)
	return fang.ColorScheme{
		Base:           base,
		Title:          accent,
		Description:    base,
		Codeblock:      lipgloss.Color(p.base200),
		Program:        accent,
		DimmedArgument: dim,
		Comment:        dim,
		Flag:           flag,
		FlagDefault:    dim,
		Command:        command,
		QuotedString:   quoted,
		Argument:       base,
		ErrorHeader:    [2]color.Color{lipgloss.Color(p.errContent), lipgloss.Color(p.errColor)},
	}
}

// slateColorScheme adapts the Slate themes for fang: fang hands us a
// light/dark selector (driven by lipgloss's terminal-background detection) and
// we resolve each role through it — the same per-field pattern fang's own
// DefaultColorScheme uses. All further degradation — NO_COLOR, 256/16-color,
// piped output — is handled by fang's colorprofile pipeline from there.
func slateColorScheme(c lipgloss.LightDarkFunc) fang.ColorScheme {
	light, dark := slateLight.colorScheme(), slateDark.colorScheme()
	return fang.ColorScheme{
		Base:           c(light.Base, dark.Base),
		Title:          c(light.Title, dark.Title),
		Description:    c(light.Description, dark.Description),
		Codeblock:      c(light.Codeblock, dark.Codeblock),
		Program:        c(light.Program, dark.Program),
		DimmedArgument: c(light.DimmedArgument, dark.DimmedArgument),
		Comment:        c(light.Comment, dark.Comment),
		Flag:           c(light.Flag, dark.Flag),
		FlagDefault:    c(light.FlagDefault, dark.FlagDefault),
		Command:        c(light.Command, dark.Command),
		QuotedString:   c(light.QuotedString, dark.QuotedString),
		Argument:       c(light.Argument, dark.Argument),
		ErrorHeader: [2]color.Color{
			c(light.ErrorHeader[0], dark.ErrorHeader[0]),
			c(light.ErrorHeader[1], dark.ErrorHeader[1]),
		},
		// Help, Dash and ErrorDetails stay unset — see colorScheme above.
	}
}
