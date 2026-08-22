package spam

import (
	"reflect"
	"testing"
)

func TestURLsAndDomains(t *testing.T) {
	body := "Hi Jon, see https://bit.ly/3xYz. Also www.Solar-Quotes.example/offer?a=1) and acme.example/x"
	got := URLs(body)
	want := []string{"https://bit.ly/3xYz", "www.Solar-Quotes.example/offer?a=1", "acme.example/x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("URLs = %q, want %q", got, want)
	}
	// The trailing period must not become part of the host, or a shortener
	// denylist silently stops matching at the end of a sentence.
	if d := DomainOf(got[0]); d != "bit.ly" {
		t.Errorf("DomainOf = %q, want bit.ly", d)
	}
	if d := DomainOf(got[1]); d != "solar-quotes.example" {
		t.Errorf("DomainOf dropped www/case: %q", d)
	}
}

func TestPhonesNormalizesAndRejectsGluedDigits(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"national", "call 555-123-4567 now", []string{"+15551234567"}},
		{"parens", "call (555) 123 4567", []string{"+15551234567"}},
		{"with country code", "+1 555.123.4567", []string{"+15551234567"}},
		{"deduped", "555-123-4567 or 5551234567", []string{"+15551234567"}},
		{"order number is not a phone", "order 12345551234567890", nil},
		{"inside a url", "https://x.example/15551234567", nil},
		{"inside an email", "a15551234567@x.example", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Phones(tc.body); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Phones(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestNamesUsedRespectsWordBoundaries(t *testing.T) {
	variants := []string{"Jon", "Johnny"}
	if got := NamesUsed("Hi Jonathan, are you there?", variants); len(got) != 0 {
		t.Errorf("Jon fired inside Jonathan: %q", got)
	}
	if got := NamesUsed("hey jon!", variants); !reflect.DeepEqual(got, []string{"Jon"}) {
		t.Errorf("case-insensitive match failed: %q", got)
	}
}

func TestEntitiesCombinesKeywordsAndHosts(t *testing.T) {
	urls := []string{"https://bit.ly/abc"}
	got := Entities("Cut your SOLAR bill", []string{"solar", "medicare"}, urls)
	want := []string{"solar", "bit.ly"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Entities = %q, want %q", got, want)
	}
}

func TestAreaCode(t *testing.T) {
	for in, want := range map[string]string{
		"+15551234567":  "555",
		"5551234567":    "555",
		"662265":        "",
		"":              "",
		"+442071234567": "",
	} {
		if got := AreaCode(in); got != want {
			t.Errorf("AreaCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeForMatchDropsPunctuationAndCase(t *testing.T) {
	if got := NormalizeForMatch("  STOP!!  "); got != "stop" {
		t.Errorf("got %q", got)
	}
	// Curly quotes and em-dashes are punctuation autocorrect inserts; they must
	// not survive into the comparison or a canned notice stops matching itself.
	if got := NormalizeForMatch("I don’t consent — stop."); got != "i don t consent stop" {
		t.Errorf("got %q", got)
	}
}
