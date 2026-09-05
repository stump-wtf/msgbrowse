package store

// Tests for DayAttachments (issue #439): the store side of resolving the
// journal card's standout-media strings to real attachments — UTC day
// bucketing per ADR-0023, exclude list honored, malformed days rejected.
//
// @joestump-agent 09/05/2026 - Added with #439.

import (
	"context"
	"testing"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

func TestDayAttachments(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sig, err := st.UpsertConversation(ctx, source.Signal, "Alex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, sig, source.Signal, []signal.Message{
		msg("Alex", "2023-05-01 10:00:00", "Alex", "with photo", []signal.Attachment{{
			Kind: signal.KindImage, RelPath: "media/2023/05/photo.jpg", OriginalName: "photo.jpg",
		}}, nil),
		msg("Alex", "2023-05-02 10:00:00", "Alex", "next day", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}

	atts, err := st.DayAttachments(ctx, "2023-05-01", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 || atts[0].OriginalName != "photo.jpg" || atts[0].ConversationID != sig {
		t.Fatalf("day attachments = %+v, want the one photo on Alex's conversation", atts)
	}

	// The adjacent day holds none.
	if atts, err = st.DayAttachments(ctx, "2023-05-02", nil); err != nil || len(atts) != 0 {
		t.Fatalf("day-2 attachments = %+v err %v, want none", atts, err)
	}

	// Exclude list drops the conversation's attachments.
	if atts, err = st.DayAttachments(ctx, "2023-05-01", []string{"Alex"}); err != nil || len(atts) != 0 {
		t.Fatalf("excluded conversation attachments = %+v err %v, want none", atts, err)
	}

	// Malformed day is rejected, not silently empty.
	if _, err = st.DayAttachments(ctx, "not-a-day", nil); err == nil {
		t.Error("malformed day must error, not return an empty set")
	}
}
