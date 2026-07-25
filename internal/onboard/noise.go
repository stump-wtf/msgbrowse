// Benign exporter-output filtering. The bundled imessage-exporter prints a
// startup warning — "No MOV converter found, video attachments will not be
// converted!" — whenever ffmpeg is absent from $PATH. But msgbrowse ALWAYS
// invokes it in copy mode (`-f txt -c clone`, see ExportArgs), and copy mode
// never converts video: MOV attachments are cloned into the archive as-is and
// played back locally. So that line is a guaranteed false alarm on EVERY
// iMessage export, ffmpeg installed or not — it once led a user to think a MOV
// converter had to be installed to fix a failing export (the real failure was a
// Full Disk Access wall on the same run). We strip it from the captured output
// before it reaches the JobLog the enable card and Logs viewer render, so the
// diagnostic surface shows only lines that mean something for this app.
//
// This removes ONE exact, always-benign line and nothing else: every other
// exporter line — progress, real errors, the permission-wall hint the classifier
// keys on — passes through untouched, so filtering can never hide a genuine
// failure. It is display hygiene layered over the raw capture, not a substitute
// for it.
package onboard

import "strings"

// benignExporterNoise are exact exporter output lines that are structurally
// guaranteed irrelevant to msgbrowse (the tool warns about a capability we never
// ask it to use) and are dropped from captured output before it is surfaced. Add
// a new always-benign line here; anything conditional does NOT belong in this
// list, because a line that is only sometimes noise can hide a real signal.
var benignExporterNoise = []string{
	// imessage-exporter's ffmpeg-absent warning. Irrelevant under `-c clone`,
	// which copies attachments verbatim and converts nothing (see ExportArgs).
	"No MOV converter found, video attachments will not be converted!",
}

// stripExporterNoise removes known always-benign exporter warning lines from an
// exporter's captured combined output, preserving every other line and the
// original order. Matching is on the trimmed line, so surrounding whitespace does
// not defeat it. It is safe to run on any exporter's output: a source whose tool
// never prints these lines is returned unchanged. It also has no effect on
// permission classification — the stripped lines contain no permission pattern —
// so filtering before classifyExportFailure cannot mask a permission wall.
func stripExporterNoise(output string) string {
	if output == "" {
		return ""
	}
	lines := strings.Split(output, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if isBenignExporterNoise(line) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// isBenignExporterNoise reports whether a single output line is one of the known
// always-benign exporter warnings, comparing on the trimmed line.
func isBenignExporterNoise(line string) bool {
	trimmed := strings.TrimSpace(line)
	for _, noise := range benignExporterNoise {
		if trimmed == noise {
			return true
		}
	}
	return false
}
