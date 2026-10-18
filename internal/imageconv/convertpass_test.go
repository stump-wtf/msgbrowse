package imageconv

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// These tests run an actual conversion pass through Detect + convert with a
// FAKE converter standing in on PATH, so CI needs no ImageMagick, no sips and no
// libheif (issue #59): the distroless runtime image ships none of them, and the
// end-to-end onboard path already proves a real run wires up to the store
// (internal/onboardsvc/transcode_test.go). What is exercised here is the unit
// the pipeline hands to the tool: converter discovery, the src→dst argument
// order, the atomic temp-file rename, and the shape of what lands in the
// derivative cache.
//
// Environmental dependency: /bin/sh and /bin/cp (absolute paths, since the
// stripped PATH cannot resolve them). Nothing else on the host is required.

const (
	fixtureWidth  = 16
	fixtureHeight = 8
)

// writeGoldenJPEG encodes a small real JPEG and returns its path. The fake
// converter emits exactly these bytes for any source it is handed, which is what
// lets the test assert the derivative decodes as a JPEG of known dimensions
// without a real transcoder being present.
func writeGoldenJPEG(t *testing.T) string {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, fixtureWidth, fixtureHeight))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "golden.jpg")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// installFakeMagick puts one `magick` script on an otherwise empty PATH, so
// Detect deterministically picks it (on macOS the real sips would otherwise win)
// and no host converter can be reached.
func installFakeMagick(t *testing.T, body string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "magick"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

// fakeConverterWithGolden sets up a fake that refuses a source it cannot stat
// (so a src/dst wiring regression fails loudly), records every source it was
// asked to convert in $FAKE_CALLS, and emits the golden JPEG at its output path.
func fakeConverterWithGolden(t *testing.T) (Converter, string) {
	t.Helper()
	golden := writeGoldenJPEG(t)
	calls := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKE_CALLS", calls)
	installFakeMagick(t, fmt.Sprintf(`[ -f "$1" ] || { echo "fake magick: no such source: $1" >&2; exit 2; }
printf '%%s\n' "$1" >> "$FAKE_CALLS"
exec /bin/cp '%s' "$2"`, golden))

	conv, ok := Detect()
	if !ok {
		t.Fatal("Detect found no converter with the fake magick on PATH")
	}
	if conv.Name != "magick" {
		t.Fatalf("Detect picked %q, want the fake magick", conv.Name)
	}
	return conv, calls
}

// writeHEICSource writes a file whose NAME is what matters: only the extension
// decides that it is convertible, and the fake converter ignores the bytes.
func writeHEICSource(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "IMG_0001.heic")
	if err := os.WriteFile(src, []byte("heic fixture bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !Convertible(src) {
		t.Fatalf("fixture %s is not classified as convertible", src)
	}
	return src
}

// TestConversionPassWritesDecodableJPEGDerivative is the unit-level conversion
// pass issue #59 asked for: a source in the cache is keyed by its path, handed
// to the converter as the first argument, and the derivative that appears at
// DerivedPath is a real decodable JPEG of the expected dimensions.
func TestConversionPassWritesDecodableJPEGDerivative(t *testing.T) {
	conv, calls := fakeConverterWithGolden(t)
	src := writeHEICSource(t)

	dataDir := t.TempDir()
	dst := DerivedPath(DerivedDir(dataDir), src)
	if err := conv.convert(context.Background(), src, dst); err != nil {
		t.Fatalf("convert: %v", err)
	}

	f, err := os.Open(dst)
	if err != nil {
		t.Fatalf("open derivative: %v", err)
	}
	defer func() { _ = f.Close() }()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("derivative is not a decodable image: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("derivative decodes as %q, want jpeg", format)
	}
	if cfg.Width != fixtureWidth || cfg.Height != fixtureHeight {
		t.Errorf("derivative dimensions = %dx%d, want %dx%d",
			cfg.Width, cfg.Height, fixtureWidth, fixtureHeight)
	}

	// The converter is called as `magick <src> <out>`: it must have been handed
	// the absolute source, not the destination or the temp file.
	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read converter log: %v", err)
	}
	if got := strings.TrimSpace(string(logged)); got != src {
		t.Errorf("converter was handed %q, want the source %q", got, src)
	}

	// The rename is atomic per call, so the cache dir holds the derivative and
	// no in-flight conv-*.jpg leftovers.
	entries, err := os.ReadDir(DerivedDir(dataDir))
	if err != nil {
		t.Fatalf("read derived dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(dst) {
		t.Errorf("derived dir holds %v, want just the derivative", names(entries))
	}
}

// TestConverterFailureLeavesNoDerivative pins the failure contract: the error
// names the tool and carries its output, no derivative is published, and the
// temp file is cleaned up rather than left in the cache to be served later.
func TestConverterFailureLeavesNoDerivative(t *testing.T) {
	installFakeMagick(t, `echo "fake magick: cannot decode $1" >&2
exit 1`)
	conv, ok := Detect()
	if !ok || conv.Name != "magick" {
		t.Fatalf("Detect = %+v/%v, want the fake magick", conv, ok)
	}
	src := writeHEICSource(t)

	dataDir := t.TempDir()
	dst := DerivedPath(DerivedDir(dataDir), src)
	err := conv.convert(context.Background(), src, dst)
	if err == nil {
		t.Fatal("convert succeeded although the converter exits non-zero")
	}
	if !strings.Contains(err.Error(), "magick") {
		t.Errorf("error %q does not name the failing converter", err)
	}
	if !strings.Contains(err.Error(), "cannot decode") {
		t.Errorf("error %q lost the converter's own output", err)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Errorf("failed conversion published a derivative at %s", dst)
	}
	entries, err := os.ReadDir(DerivedDir(dataDir))
	if err != nil {
		t.Fatalf("read derived dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("failed conversion left %v in the cache dir", names(entries))
	}
}

// TestConcurrentConvertsToOneDerivative covers the case convert's comment
// describes: one source path referenced by several attachment rows, so several
// workers target the same destination at once. Per-call temp names must keep
// every write valid and leave the cache holding one clean derivative.
func TestConcurrentConvertsToOneDerivative(t *testing.T) {
	conv, _ := fakeConverterWithGolden(t)
	src := writeHEICSource(t)

	dataDir := t.TempDir()
	dst := DerivedPath(DerivedDir(dataDir), src)

	const writers = 4
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = conv.convert(context.Background(), src, dst)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatalf("open derivative: %v", err)
	}
	defer func() { _ = f.Close() }()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("derivative is not a decodable image after concurrent writes: %v", err)
	}
	if format != "jpeg" || cfg.Width != fixtureWidth || cfg.Height != fixtureHeight {
		t.Errorf("derivative = %s %dx%d, want jpeg %dx%d", format, cfg.Width, cfg.Height,
			fixtureWidth, fixtureHeight)
	}
	entries, err := os.ReadDir(DerivedDir(dataDir))
	if err != nil {
		t.Fatalf("read derived dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(dst) {
		t.Errorf("derived dir holds %v, want just the derivative", names(entries))
	}
}

// TestDetectWithoutConverterOnPath is the assumption behind Run's
// NoConverter=true no-op: with no sips / magick / convert / heif-convert on
// PATH, detection reports nothing rather than picking a broken candidate.
func TestDetectWithoutConverterOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if conv, ok := Detect(); ok {
		t.Errorf("Detect = %+v/true with an empty PATH, want no converter", conv)
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
