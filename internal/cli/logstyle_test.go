package cli

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// Guards the emoji level labels through the charm.land/log/v2 migration: the
// v1 module set MaxWidth 4 on level styles, which truncated "✅ INFO" to
// "✅ I". A compile-only check would not have caught that.
func TestCharmLoggerRendersEmojiLevels(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	l := newCharmLogger("debug")
	slog.New(l).Info("hello")
	slog.New(l).Warn("careful")
	slog.New(l).Error("boom")
	w.Close()
	os.Stderr = old
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	for _, want := range []string{"✅ INFO", "⚠️ WARN", "🛑 ERROR", "hello", "careful", "boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("logger output missing %q\ngot: %s", want, out)
		}
	}
}
