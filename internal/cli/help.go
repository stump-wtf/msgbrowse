package cli

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"iter"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"charm.land/fang/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// This file is msgbrowse's help renderer: fang's layout (aligned command/flag
// tables, usage codeblock, uppercase section titles — the "Crush look" adopted
// in #331) reproduced on top of fang's *exported* API, so we can fix three
// defects that fang v2.0.1 gives us no option to switch off (#333):
//
//   - Latency. fang's help path calls the unexported mustColorscheme, which
//     runs lipgloss.HasDarkBackground — a synchronous OSC 11 query with a ~4s
//     timeout — whenever stdout is a tty. Any pty that does not answer (script,
//     asciinema, editor terminals, `docker run -t`) paid the full 4.03s on every
//     `--help`. terminalIsDark below resolves light/dark from the environment
//     instead, so nothing is ever written to the terminal to ask.
//
//   - Flag value types. fang's flag table renders the flag name only, dropping
//     pflag's value-type placeholder: `--port` instead of `--port int`, and
//     `--imessage-exporter-args` instead of `--imessage-exporter-args
//     -- <args>`. evalFlags below goes through pflag.UnquoteUsage, the same
//     call pflag's own FlagUsages uses, so the placeholder (and cobra's
//     `(default X)` wording) comes back.
//
//   - Literal casing. fang applies Transform(titleFirstWord) to
//     styles.FlagDescription, which renders both flag descriptions and command
//     Short strings. That rewrites real, case-sensitive executable names —
//     `sigexport-only` became `Sigexport-Only` — and fang exposes no option to
//     unset it (#331 could only reach styles.ErrorText). slateStyles below
//     simply never installs the transform.
//
// Trailing whitespace (the fourth defect) falls out of owning the writer: fang
// renders through lipgloss styles whose width padding survives colorprofile's
// ANSI stripping, so piped help came out right-padded on every rendered line.
// renderHelp buffers the styled output and trims each line before it reaches
// the caller's writer.
//
// The pieces below deliberately mirror charm.land/fang/v2@v2.0.1's help.go and
// theme.go so the rendering stays recognizably fang's. Keep them in sync when
// the dependency moves.

const (
	// minSpace is the floor on the gutter between a command/flag key column
	// and its description; shortPad and longPad are fang's indents for the
	// long/short paragraph and the table keys respectively.
	minSpace = 10
	shortPad = 2
	longPad  = 4

	// maxHelpWidth caps the rendered width, and doubles as the width used when
	// the output is not a terminal (a pipe, a file, or a test buffer).
	maxHelpWidth = 120
)

// installHelp points every command in the tree at renderHelp. Cobra resolves
// HelpFunc by walking to the root, so setting it once on the root covers the
// whole tree — including the `help` subcommand, which calls the same hook.
func installHelp(root *cobra.Command) {
	root.SetHelpFunc(func(c *cobra.Command, _ []string) {
		renderHelp(c, c.OutOrStdout())
	})
}

// helpWidth is the render width for out: the terminal's own width (capped at
// maxHelpWidth) when out is a terminal, and maxHelpWidth otherwise. fang used
// os.Stdout unconditionally and memoized the answer process-wide; asking the
// actual destination keeps `msgbrowse --help > file` and the tests honest.
func helpWidth(out io.Writer) int {
	f, ok := out.(term.File)
	if !ok {
		return maxHelpWidth
	}
	w, _, err := term.GetSize(f.Fd())
	if err != nil || w <= 0 {
		return maxHelpWidth
	}
	return min(w, maxHelpWidth)
}

// terminalIsDark reports whether help should render against the dark half of
// the Slate palette.
//
// This is the M2 fix: it must never write to the terminal. lipgloss's
// HasDarkBackground writes an OSC 11 query and blocks for ~4s waiting on a
// reply that many pseudo-terminals never send, and fang calls it on every help
// invocation with no way to opt out. The only cost-free signal available is
// COLORFGBG (exported by rxvt, urxvt, Konsole and several others) whose second
// field is the background's ANSI index; 7 and 15 are the light ones. With no
// signal we assume dark, which is both the common case and lipgloss's own
// fallback when the query fails.
func terminalIsDark() bool {
	_, bg, ok := strings.Cut(os.Getenv("COLORFGBG"), ";")
	if !ok {
		return true
	}
	// Some terminals export three fields (fg;bold;bg); the background is
	// always the last one.
	if _, last, more := strings.Cut(bg, ";"); more {
		bg = last
	}
	switch n, err := strconv.Atoi(strings.TrimSpace(bg)); {
	case err != nil:
		return true
	default:
		return n != 7 && n != 15
	}
}

// slateStyles builds fang's Styles from a ColorScheme. It is a copy of fang's
// unexported makeStyles with two deliberate differences, both of them the
// point of this shim:
//
//   - FlagDescription carries no Transform(titleFirstWord), so flag
//     descriptions and command Short strings render verbatim (M1).
//   - ErrorText carries no transform either, which is what slateErrorHandler
//     was already asking for with UnsetTransform. Note fang gives ErrorText no
//     Foreground at all, so error text renders in the terminal's default color
//     regardless of the scheme.
func slateStyles(cs fang.ColorScheme, width int) fang.Styles {
	return fang.Styles{
		Text: lipgloss.NewStyle().Foreground(cs.Base),
		Title: lipgloss.NewStyle().
			Bold(true).
			Foreground(cs.Title).
			Transform(strings.ToUpper).
			Padding(1, 0).
			Margin(0, 2),
		FlagDescription: lipgloss.NewStyle().
			Foreground(cs.Description),
		FlagDefault: lipgloss.NewStyle().
			Foreground(cs.FlagDefault),
		Codeblock: fang.Codeblock{
			Base: lipgloss.NewStyle().
				Background(cs.Codeblock).
				Foreground(cs.Base).
				MarginLeft(2).
				Padding(1, 2),
			Text: lipgloss.NewStyle().
				Background(cs.Codeblock),
			Comment: lipgloss.NewStyle().
				Background(cs.Codeblock).
				Foreground(cs.Comment),
			Program: fang.Program{
				Name: lipgloss.NewStyle().
					Background(cs.Codeblock).
					Foreground(cs.Program),
				Flag: lipgloss.NewStyle().
					Background(cs.Codeblock).
					Foreground(cs.Flag),
				Argument: lipgloss.NewStyle().
					Background(cs.Codeblock).
					Foreground(cs.Argument),
				DimmedArgument: lipgloss.NewStyle().
					Background(cs.Codeblock).
					Foreground(cs.DimmedArgument),
				Command: lipgloss.NewStyle().
					Background(cs.Codeblock).
					Foreground(cs.Command),
				QuotedString: lipgloss.NewStyle().
					Background(cs.Codeblock).
					Foreground(cs.QuotedString),
			},
		},
		Program: fang.Program{
			Name:           lipgloss.NewStyle().Foreground(cs.Program),
			Argument:       lipgloss.NewStyle().Foreground(cs.Argument),
			DimmedArgument: lipgloss.NewStyle().Foreground(cs.DimmedArgument),
			Flag:           lipgloss.NewStyle().Foreground(cs.Flag),
			Command:        lipgloss.NewStyle().Foreground(cs.Command),
			QuotedString:   lipgloss.NewStyle().Foreground(cs.QuotedString),
		},
		Span: lipgloss.NewStyle().Background(cs.Codeblock),
		ErrorText: lipgloss.NewStyle().
			MarginLeft(2).
			Width(width - longPad),
		ErrorHeader: lipgloss.NewStyle().
			Foreground(cs.ErrorHeader[0]).
			Background(cs.ErrorHeader[1]).
			Bold(true).
			Padding(0, 1).
			Margin(1).
			MarginLeft(2).
			SetString("ERROR"),
	}
}

// styleFor resolves the Slate scheme and styles for a given output target
// without querying the terminal. Both the help and the error surfaces go
// through it so they cannot drift apart.
func styleFor(out io.Writer) (fang.Styles, int) {
	width := helpWidth(out)
	return slateStyles(slateColorScheme(lipgloss.LightDark(terminalIsDark())), width), width
}

// renderHelp writes c's help page to out in fang's layout.
func renderHelp(c *cobra.Command, out io.Writer) {
	styles, width := styleFor(out)
	profile := colorprofile.Detect(out, os.Environ())

	// Render into a buffer through a colorprofile writer pinned to the *real*
	// destination's profile, so color degradation still matches the caller
	// while the trailing-padding trim below sees the final bytes (M6).
	var buf bytes.Buffer
	w := &colorprofile.Writer{Forward: &buf, Profile: profile}
	writeHelp(c, w, styles, width, profile)
	_, _ = io.WriteString(out, trimTrailingSpace(buf.String()))
}

// trimTrailingSpace strips run-of-the-mill trailing spaces and tabs from every
// line.
//
// lipgloss pads styled blocks out to their declared width; colorprofile strips
// the color but not the padding, so piped help came out with trailing
// whitespace on every rendered line. Styled blocks that carry a background
// (the usage codeblock) end their lines with a reset sequence rather than a
// bare space, so their padding is deliberately left alone here.
func trimTrailingSpace(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}

func writeHelp(c *cobra.Command, w *colorprofile.Writer, styles fang.Styles, width int, profile colorprofile.Profile) {
	writeLongShort(w, styles, cmp.Or(c.Long, c.Short), width)

	usage := styleUsage(c, styles.Codeblock.Program, true)
	examples := styleExamples(c, styles)

	padding := styles.Codeblock.Base.GetHorizontalPadding()
	blockWidth := lipgloss.Width(usage)
	for _, ex := range examples {
		blockWidth = max(blockWidth, lipgloss.Width(ex))
	}
	blockWidth = min(width-padding, blockWidth+padding)
	blockStyle := styles.Codeblock.Base.Width(blockWidth)

	// With no color (ascii/notty) or no background there is no block to pad
	// out, so drop the vertical padding — otherwise it reads as stray blank
	// lines. This mirrors fang.
	if profile <= colorprofile.ASCII || reflect.DeepEqual(blockStyle.GetBackground(), lipgloss.NoColor{}) {
		blockStyle = blockStyle.PaddingTop(0).PaddingBottom(0)
	}

	_, _ = fmt.Fprintln(w, styles.Title.Render("usage"))
	_, _ = fmt.Fprintln(w, blockStyle.Render(usage))
	if len(examples) > 0 {
		_, _ = fmt.Fprintln(w, styles.Title.Render("examples"))
		_, _ = fmt.Fprintln(w, blockStyle.Render(strings.Join(examples, "\n")))
	}

	groups, groupKeys := evalGroups(c)
	cmds, cmdKeys := evalCmds(c, styles)
	flags, flagKeys := evalFlags(c, styles)
	space := calculateSpace(cmdKeys, flagKeys)

	for _, groupID := range groupKeys {
		group := cmds[groupID]
		if len(group) == 0 {
			continue
		}
		renderGroup(w, styles, space, groups[groupID], func(yield func(string, string) bool) {
			for _, k := range cmdKeys {
				help, ok := group[k]
				if !ok {
					continue
				}
				if !yield(k, help) {
					return
				}
			}
		})
	}

	if len(flags) > 0 {
		renderGroup(w, styles, space, "flags", func(yield func(string, string) bool) {
			for _, k := range flagKeys {
				if !yield(k, flags[k]) {
					return
				}
			}
		})
	}

	_, _ = fmt.Fprintln(w)
}

func writeLongShort(w io.Writer, styles fang.Styles, longShort string, width int) {
	if longShort == "" {
		return
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, styles.Text.Width(width).PaddingLeft(shortPad).Render(longShort))
}

var otherArgsRe = regexp.MustCompile(`(\[.*\])`)

// styleUsage renders a command's usage line: program name, subcommand path,
// then the dimmed [command] / [args] / [--flags] placeholders. Ported from
// fang so the usage codeblock and the COMMANDS keys keep its shape.
func styleUsage(c *cobra.Command, styles fang.Program, complete bool) string {
	u := c.Use
	if complete {
		u = c.UseLine()
	}
	hasArgs := strings.Contains(u, "[args]")
	hasFlags := strings.Contains(u, "[flags]") || strings.Contains(u, "[--flags]") ||
		c.HasFlags() || c.HasPersistentFlags() || c.HasAvailableFlags()
	hasCommands := strings.Contains(u, "[command]") || c.HasAvailableSubCommands()
	for _, k := range []string{"[args]", "[flags]", "[--flags]", "[command]"} {
		u = strings.ReplaceAll(u, k, "")
	}

	var otherArgs []string
	for _, arg := range otherArgsRe.FindAllString(u, -1) {
		u = strings.ReplaceAll(u, arg, "")
		otherArgs = append(otherArgs, arg)
	}

	u = strings.TrimSpace(u)

	var useLine []string
	if complete {
		parts := strings.Fields(u)
		useLine = append(useLine, styles.Name.Render(parts[0]))
		if len(parts) > 1 {
			useLine = append(useLine, styles.Command.Render(" "+strings.Join(parts[1:], " ")))
		}
	} else {
		useLine = append(useLine, styles.Command.Render(u))
	}
	if hasCommands {
		useLine = append(useLine, styles.DimmedArgument.Render(" [command]"))
	}
	if hasArgs {
		useLine = append(useLine, styles.DimmedArgument.Render(" [args]"))
	}
	for _, arg := range otherArgs {
		useLine = append(useLine, styles.DimmedArgument.Render(" "+arg))
	}
	if hasFlags {
		useLine = append(useLine, styles.DimmedArgument.Render(" [--flags]"))
	}
	return lipgloss.JoinHorizontal(lipgloss.Left, useLine...)
}

// styleExamples renders cmd.Example inside the usage codeblock. fang
// syntax-highlights each shell token; we render the lines plainly and only
// dim `# ` comments, which is all msgbrowse needs — no command in the tree
// sets Example today, and re-deriving fang's tokenizer would be a second copy
// to keep in sync for no benefit.
func styleExamples(c *cobra.Command, styles fang.Styles) []string {
	if c.Example == "" {
		return nil
	}
	var out []string
	lines := strings.Split(c.Example, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if (i == 0 || i == len(lines)-1) && line == "" {
			continue
		}
		if strings.HasPrefix(line, "# ") {
			out = append(out, styles.Codeblock.Comment.Render(line))
			continue
		}
		out = append(out, styles.Codeblock.Text.Render(line))
	}
	return out
}

// evalFlags builds the FLAGS table.
//
// Unlike fang's, the key carries pflag's value-type placeholder and the
// default is spelled the way cobra spells it (M3). Both come from
// pflag.UnquoteUsage, which is exactly what pflag's own FlagUsages calls: it
// returns the backquoted name from the usage string when there is one
// (`sigexport`, `-- <args>`), and the flag's value type otherwise (int,
// string, …), with bool flags deliberately yielding no placeholder.
func evalFlags(c *cobra.Command, styles fang.Styles) (map[string]string, []string) {
	flags := map[string]string{}
	var keys []string
	c.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		name := "--" + f.Name
		if f.Shorthand != "" {
			name = "-" + f.Shorthand + " --" + f.Name
		}
		varname, usage := pflag.UnquoteUsage(f)
		parts := []string{styles.Program.Flag.Render(name)}
		if varname != "" {
			parts = append(parts, styles.Program.DimmedArgument.Render(" "+varname))
		}
		key := lipgloss.JoinHorizontal(lipgloss.Left, parts...)

		help := styles.FlagDescription.Render(usage)
		if def, ok := flagDefault(f); ok {
			help += styles.FlagDefault.Render(" (default " + def + ")")
		}
		flags[key] = help
		keys = append(keys, key)
	})
	return flags, keys
}

// flagDefault reports the default worth printing for f, if any. pflag's own
// zero-value test is unexported, so this reproduces its observable behaviour:
// skip the zero of each kind, and quote string defaults the way pflag does.
func flagDefault(f *pflag.Flag) (string, bool) {
	switch f.DefValue {
	case "", "false", "0", "0s", "[]", "map[]":
		return "", false
	}
	if f.Value.Type() == "string" {
		return strconv.Quote(f.DefValue), true
	}
	return f.DefValue, true
}

// evalCmds builds the per-group COMMANDS tables: map[groupID]map[key]help,
// plus the keys in declaration order. Short strings render verbatim (M1).
func evalCmds(c *cobra.Command, styles fang.Styles) (map[string]map[string]string, []string) {
	var keys []string
	cmds := map[string]map[string]string{}
	for _, sc := range c.Commands() {
		if sc.Hidden || !sc.IsAvailableCommand() {
			continue
		}
		if _, ok := cmds[sc.GroupID]; !ok {
			cmds[sc.GroupID] = map[string]string{}
		}
		key := styleUsage(sc, styles.Program, false)
		cmds[sc.GroupID][key] = styles.FlagDescription.Render(sc.Short)
		keys = append(keys, key)
	}
	return cmds, keys
}

func evalGroups(c *cobra.Command) (map[string]string, []string) {
	// The default (ungrouped) bucket always comes first.
	ids := make([]string, 1, 1+len(c.Groups()))
	ids[0] = ""
	groups := map[string]string{"": "commands"}
	for _, g := range c.Groups() {
		groups[g.ID] = g.Title
		ids = append(ids, g.ID)
	}
	return groups, ids
}

func renderGroup(w io.Writer, styles fang.Styles, space int, name string, items iter.Seq2[string, string]) {
	_, _ = fmt.Fprintln(w, styles.Title.Render(name))
	for key, help := range items {
		_, _ = fmt.Fprintln(w, lipgloss.JoinHorizontal(
			lipgloss.Left,
			lipgloss.NewStyle().PaddingLeft(longPad).Render(key),
			strings.Repeat(" ", max(space-lipgloss.Width(key), 1)),
			help,
		))
	}
}

func calculateSpace(k1, k2 []string) int {
	const spaceBetween = 2
	space := minSpace
	for _, k := range append(append([]string{}, k1...), k2...) {
		space = max(space, lipgloss.Width(k)+spaceBetween)
	}
	return space
}
