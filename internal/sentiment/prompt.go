package sentiment

import (
	"fmt"
	"strings"

	"github.com/joestump/msgbrowse/internal/store"
)

// systemPrompt renders the instruction half of the scoring call, including the
// lexicon's constructs and their keyed anchor items.
//
// Two things in here are requirements rather than style, and should not be
// softened without re-reading SPEC-0027:
//
//   - The task is framed as scoring *what a message expresses*, never as
//     assessing the person who wrote it. This feature builds something
//     dossier-shaped out of real people's messages; the framing is the first of
//     several guards (see also the UI disclaimers in #313) that keep it honest
//     about what it is measuring.
//   - The model is told to OMIT constructs it has no salient evidence for.
//     Storage is sparse by design: a corpus of "ok, see you at 6" should produce
//     no rows at all, so the table stays proportional to expressive content
//     rather than to message count.
func systemPrompt(lex *Lexicon) string {
	var b strings.Builder
	b.WriteString(`You score what a message EXPRESSES against a fixed list of constructs.

You are scoring text, not people. Do not diagnose, assess, or draw conclusions about the author. Score only what a specific message expresses.

Rules:
- Return ONLY a JSON array, no prose, no markdown fences.
- Each element is an object: {"message": integer, "scores": {"<construct>": number}}.
- "message" is the 1-based number of the message being scored.
- Keys of "scores" MUST come from the construct list below, spelled exactly.
- Each score is a number from -1.0 to +1.0. Positive means the message expresses
  the construct in the direction its "+" anchors describe; negative means the
  direction its "-" anchors describe.
- OMIT any construct you have no clear evidence for in that message. Most
  messages express few constructs or none.
- OMIT a message entirely if it expresses nothing on this list. Small talk,
  logistics, and acknowledgements usually score nothing.
- Use the surrounding messages only as context for reading tone (sarcasm, quoted
  text, running jokes). Score each message on its own.

Constructs:
`)
	for _, c := range lex.Constructs {
		fmt.Fprintf(&b, "\n- %s (%s)\n", c.Name, c.Tier)
		for _, a := range c.Anchors {
			sign := "+"
			if a.Key == -1 {
				sign = "-"
			}
			fmt.Fprintf(&b, "    %s %s\n", sign, a.Text)
		}
	}
	return b.String()
}

// buildPrompt renders the numbered message batch. The 1-based position is the
// index the model cites back, mirroring the facts extractor so both surfaces
// read the same way. The owner is labeled "You" so the model can tell the two
// sides of a conversation apart.
func buildPrompt(contact string, included []store.MessageView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Conversation with: %s\n\nMessages:\n", contact)
	for i, m := range included {
		who := contact
		if m.IsOwner {
			who = "You"
		}
		date := m.TS
		if len(date) >= 10 {
			date = date[:10]
		}
		fmt.Fprintf(&b, "%d. [%s] %s: %s\n", i+1, date, who, strings.TrimSpace(m.Body))
	}
	return b.String()
}
