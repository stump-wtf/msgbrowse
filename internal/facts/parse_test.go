package facts

import (
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/store"
)

func included() []store.MessageView {
	return []store.MessageView{
		{ID: 1, Hash: "h1", Sender: "Harper", TS: "2023-05-01 10:00:00", TSUnix: 100, Body: "i adopted a dog"},
		{ID: 2, Hash: "h2", Sender: "Me", IsOwner: true, TS: "2023-05-01 10:01:00", TSUnix: 101, Body: "nice!"},
		{ID: 3, Hash: "h3", Sender: "Harper", TS: "2023-05-02 09:00:00", TSUnix: 200, Body: "i'm a nurse"},
	}
}

func TestParseFactsFencedAndBound(t *testing.T) {
	raw := "Sure! Here you go:\n```json\n[" +
		`{"fact":"Has a dog","category":"personal","evidence":1},` +
		`{"fact":"Works as a nurse","category":"WORK","evidence":3}` +
		"]\n```\n"
	got, err := parseFacts(raw, included())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d facts, want 2", len(got))
	}
	if got[0].Fact != "Has a dog" || got[0].Msg.Hash != "h1" {
		t.Errorf("fact[0] = %+v, want bound to h1", got[0])
	}
	// Category is normalized to lowercase and bound to the cited message.
	if got[1].Category != "work" || got[1].Msg.Hash != "h3" {
		t.Errorf("fact[1] = %+v, want category work bound to h3", got[1])
	}
}

func TestParseFactsCoercesUnknownCategoryAndClampsEvidence(t *testing.T) {
	raw := `[{"fact":"Likes jazz","category":"musical taste","evidence":99},` +
		`{"fact":"No evidence given","category":"personal","evidence":0}]`
	got, err := parseFacts(raw, included())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d facts, want 2", len(got))
	}
	if got[0].Category != "other" {
		t.Errorf("unknown category not coerced: %q", got[0].Category)
	}
	// Out-of-range (99) and missing (0) evidence both clamp to the last message.
	last := "h3"
	if got[0].Msg.Hash != last || got[1].Msg.Hash != last {
		t.Errorf("evidence clamp: hashes %q,%q want both %q", got[0].Msg.Hash, got[1].Msg.Hash, last)
	}
}

func TestParseFactsEmptyAndGarbage(t *testing.T) {
	if got, err := parseFacts(`[]`, included()); err != nil || len(got) != 0 {
		t.Errorf("empty array: got %v err %v, want none", got, err)
	}
	if got, err := parseFacts(``, included()); err == nil {
		t.Errorf("no-array response should error, got %v", got)
	}
	// Blank fact strings are dropped.
	if got, err := parseFacts(`[{"fact":"   ","category":"personal","evidence":1}]`, included()); err != nil || len(got) != 0 {
		t.Errorf("blank fact: got %v err %v, want none", got, err)
	}
	// No messages to cite → nothing to parse.
	if got, err := parseFacts(`[{"fact":"x","category":"personal","evidence":1}]`, nil); err != nil || got != nil {
		t.Errorf("no included messages: got %v err %v, want nil", got, err)
	}
}

func TestBuildPromptLabelsOwnerAndNumbers(t *testing.T) {
	p := buildPrompt("Harper", included())
	if !strings.Contains(p, "Contact: Harper") {
		t.Errorf("prompt missing contact header:\n%s", p)
	}
	if !strings.Contains(p, "1. [2023-05-01] Harper: i adopted a dog") {
		t.Errorf("prompt missing numbered contact line:\n%s", p)
	}
	if !strings.Contains(p, "2. [2023-05-01] You: nice!") {
		t.Errorf("owner not labeled 'You':\n%s", p)
	}
}
