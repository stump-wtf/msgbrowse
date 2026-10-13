// Tap-Bump Reconciler Tests
//
// tap-bump.sh is the only thing standing between a merged fix and `brew
// upgrade`, and it used to be untestable: it lived as inline YAML in a job
// that needed a real tag, a real macOS build and a real tap push to exercise.
// It broke twice in five weeks and nothing noticed either time.
//
// These tests run the real script end to end against a temp dir. Every remote
// it talks to is an environment variable, so the "GitHub mirror" is a local
// git repo, its release assets are file:// URLs, and the tap is a bare repo
// the script really clones and really pushes to.
//
// @joestump 10/13/2026 - Added with the reconciler.
package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	formulaPath = "Formula/msgbrowse.rb"
	caskPath    = "Casks/msgbrowse-desktop.rb"
)

// The tap files, cut down to the lines the script reads and rewrites plus the
// lines it must leave alone: a comment mentioning sha256, and the cask url
// that follows `version` by interpolation.
const formulaTmpl = `class Msgbrowse < Formula
  # The sha256 below is pinned by CI.
  url "https://github.com/stump-wtf/msgbrowse/releases/download/v%[1]s/msgbrowse-v%[1]s-source.tar.gz"
  sha256 "%[2]s"
  license "MIT"
end
`

const caskTmpl = `cask "msgbrowse-desktop" do
  version "%s"
  sha256 "%s"

  url "https://github.com/stump-wtf/msgbrowse/releases/download/v#{version}/msgbrowse-desktop_darwin_universal.zip"
end
`

var oldSHA = strings.Repeat("0", 64)

// fixture is one fake release train: mirror tags, release assets, the tap, and
// the canonical checkout the staleness alarm reads.
type fixture struct {
	t                             *testing.T
	root, mirror, assets, tapBare string
	canonical                     string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	for _, tool := range []string{"bash", "git", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required to test tap-bump.sh: %v", tool, err)
		}
	}
	root := t.TempDir()
	f := &fixture{
		t:         t,
		root:      root,
		mirror:    filepath.Join(root, "mirror"),
		assets:    filepath.Join(root, "assets"),
		tapBare:   filepath.Join(root, "tap.git"),
		canonical: filepath.Join(root, "canonical"),
	}
	for _, repo := range []string{f.mirror, f.canonical} {
		f.git("", "init", "-q", "-b", "main", repo)
		f.git(repo, "commit", "-q", "--allow-empty", "-m", "init")
	}
	f.git("", "init", "-q", "--bare", "-b", "main", f.tapBare)
	return f
}

// env isolates git from the developer's own config: a global insteadOf, a
// commit hook or signing requirement would otherwise decide these tests.
func (f *fixture) env(extra ...string) []string {
	return append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.root,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	}, extra...)
}

func (f *fixture) gitEnv(dir string, extra []string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = f.env(extra...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	return f.gitEnv(dir, nil, args...)
}

// seedTap publishes a tap pinning the given formula and cask versions.
func (f *fixture) seedTap(formulaVer, caskVer string) {
	f.t.Helper()
	work := filepath.Join(f.root, "tap-seed")
	f.git("", "clone", "-q", f.tapBare, work)
	f.write(filepath.Join(work, formulaPath), fmt.Sprintf(formulaTmpl, formulaVer, oldSHA))
	f.write(filepath.Join(work, caskPath), fmt.Sprintf(caskTmpl, caskVer, oldSHA))
	f.git(work, "add", "-A")
	f.git(work, "commit", "-q", "-m", "seed")
	f.git(work, "push", "-q", "origin", "HEAD:main")
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// mirrorTag puts a tag on the mirror with no release behind it — what a tag
// looks like while (or when) its desktop build has not published.
func (f *fixture) mirrorTag(tag string) {
	f.t.Helper()
	f.git(f.mirror, "tag", tag)
}

// release tags the mirror and publishes both tap assets, returning their
// sha256s (source tarball, desktop zip).
func (f *fixture) release(tag string) (string, string) {
	f.t.Helper()
	f.mirrorTag(tag)
	src, zip := "source of "+tag, "desktop app of "+tag
	f.write(filepath.Join(f.assets, tag, "msgbrowse-"+tag+"-source.tar.gz"), src)
	f.write(filepath.Join(f.assets, tag, "msgbrowse-desktop_darwin_universal.zip"), zip)
	return sum(src), sum(zip)
}

// canonicalTag creates an annotated tag in the canonical checkout, aged.
func (f *fixture) canonicalTag(tag string, age time.Duration) {
	f.t.Helper()
	date := fmt.Sprintf("@%d +0000", time.Now().Add(-age).Unix())
	f.gitEnv(f.canonical, []string{"GIT_COMMITTER_DATE=" + date},
		"tag", "-a", tag, "-m", tag)
}

// run executes the script and returns its combined output and exit error.
func (f *fixture) run(extra ...string) (string, error) {
	f.t.Helper()
	script, err := filepath.Abs("tap-bump.sh")
	if err != nil {
		f.t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = f.root
	cmd.Env = f.env(append([]string{
		"MIRROR_GIT_URL=" + f.mirror,
		"ASSET_BASE_URL=file://" + f.assets,
		"TAP_GIT_URL=" + f.tapBare,
		"CANONICAL_REPO=" + f.canonical,
		"TAP_PUSH_TOKEN=not-a-real-token",
	}, extra...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *fixture) tapFile(path string) string {
	f.t.Helper()
	return f.git(f.tapBare, "show", "main:"+path)
}

func (f *fixture) tapCommits() int {
	f.t.Helper()
	return len(strings.Split(f.git(f.tapBare, "log", "--format=%s", "main"), "\n"))
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func wantContains(t *testing.T, what, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s: missing %q in:\n%s", what, want, got)
	}
}

// The core behavior, and the exact state the tap was found in: far behind,
// with a newer tag on the mirror that never got a build. The script must land
// on the newest release that is actually installable, not the newest tag.
func TestBumpsToNewestInstallableRelease(t *testing.T) {
	f := newFixture(t)
	f.seedTap("0.8.25", "0.8.25")
	f.release("v0.8.38")
	shaSrc, shaZip := f.release("v0.8.39")
	f.mirrorTag("v0.8.40")

	if out, err := f.run(); err != nil {
		t.Fatalf("tap-bump failed: %v\n%s", err, out)
	}

	formula, cask := f.tapFile(formulaPath), f.tapFile(caskPath)
	wantContains(t, "formula url", formula, "releases/download/v0.8.39/msgbrowse-v0.8.39-source.tar.gz")
	wantContains(t, "formula sha256", formula, `sha256 "`+shaSrc+`"`)
	wantContains(t, "cask version", cask, `version "0.8.39"`)
	wantContains(t, "cask sha256", cask, `sha256 "`+shaZip+`"`)
	// Lines the rewrite must not touch.
	wantContains(t, "formula comment", formula, "# The sha256 below is pinned by CI.")
	wantContains(t, "cask url", cask, "download/v#{version}/msgbrowse-desktop_darwin_universal.zip")

	if got := f.git(f.tapBare, "log", "-1", "--format=%s", "main"); got !=
		"toil(msgbrowse): bump formula + cask to v0.8.39 (automated tap bump)" {
		t.Errorf("bump commit subject = %q", got)
	}
}

// A reconciler that is safe to run on a timer must do nothing when there is
// nothing to do.
func TestIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.seedTap("0.8.25", "0.8.25")
	f.release("v0.8.39")

	for i := range 2 {
		if out, err := f.run(); err != nil {
			t.Fatalf("run %d failed: %v\n%s", i+1, err, out)
		}
	}
	if got := f.tapCommits(); got != 2 {
		t.Errorf("tap has %d commits after two runs, want 2 (seed + one bump)", got)
	}
}

// v0.8.1 shipped with the formula bumped and the cask left on v0.8.0. The two
// files converge independently of how they got out of step.
func TestHealsACaskLeftBehind(t *testing.T) {
	f := newFixture(t)
	f.seedTap("0.8.39", "0.8.25")
	f.release("v0.8.39")

	if out, err := f.run(); err != nil {
		t.Fatalf("tap-bump failed: %v\n%s", err, out)
	}
	wantContains(t, "cask version", f.tapFile(caskPath), `version "0.8.39"`)
}

func TestNeverMovesTheTapBackwards(t *testing.T) {
	f := newFixture(t)
	f.seedTap("0.9.0", "0.9.0")
	f.release("v0.8.39")

	out, err := f.run()
	if err != nil {
		t.Fatalf("tap-bump failed: %v\n%s", err, out)
	}
	wantContains(t, "output", out, "leaving it alone")
	if got := f.tapCommits(); got != 1 {
		t.Errorf("tap has %d commits, want 1 — the tap was rewritten", got)
	}
}

// The alarm. A tag that is hours old with no installable release means the
// train is broken upstream; the run must go red — but only after shipping
// what it can, so one broken hop does not also freeze the tap.
func TestFailsWhenTheTapIsStale(t *testing.T) {
	f := newFixture(t)
	f.seedTap("0.8.25", "0.8.25")
	f.release("v0.8.39")
	f.canonicalTag("v0.8.39", 48*time.Hour)
	f.canonicalTag("v0.8.41", 5*time.Hour)

	out, err := f.run()
	if err == nil {
		t.Fatalf("tap-bump succeeded with a 5h-old unshipped tag:\n%s", out)
	}
	wantContains(t, "output", out, "v0.8.41 was tagged 5h ago")
	wantContains(t, "cask version", f.tapFile(caskPath), `version "0.8.39"`)
}

// A tag minutes old is a release in progress, not an outage.
func TestAFreshTagIsNotStale(t *testing.T) {
	f := newFixture(t)
	f.seedTap("0.8.25", "0.8.25")
	f.release("v0.8.39")
	f.canonicalTag("v0.8.40", 10*time.Minute)

	if out, err := f.run(); err != nil {
		t.Fatalf("tap-bump failed on a 10-minute-old tag: %v\n%s", err, out)
	}
}

// The post-release fast path names the tag it expects. Timing out on it must
// fail the run even though the tag is far too young for the staleness alarm.
func TestFailsWhenTheAwaitedTagNeverShips(t *testing.T) {
	f := newFixture(t)
	f.seedTap("0.8.25", "0.8.25")
	f.release("v0.8.39")
	f.mirrorTag("v0.8.40")

	out, err := f.run("WAIT_FOR_TAG=v0.8.40", "WAIT_TIMEOUT=0", "WAIT_INTERVAL=0")
	if err == nil {
		t.Fatalf("tap-bump succeeded though v0.8.40 never shipped:\n%s", out)
	}
	wantContains(t, "output", out, "v0.8.40 never became installable")
	wantContains(t, "cask version", f.tapFile(caskPath), `version "0.8.39"`)
}

func TestWaitsForTheAwaitedTag(t *testing.T) {
	f := newFixture(t)
	f.seedTap("0.8.25", "0.8.25")
	f.release("v0.8.40")

	if out, err := f.run("WAIT_FOR_TAG=v0.8.40", "WAIT_TIMEOUT=0", "WAIT_INTERVAL=0"); err != nil {
		t.Fatalf("tap-bump failed: %v\n%s", err, out)
	}
	wantContains(t, "cask version", f.tapFile(caskPath), `version "0.8.40"`)
}

// A missing token used to mean a silently skipped bump. It is fatal, and it is
// fatal before anything is cloned or rewritten.
func TestRequiresThePushToken(t *testing.T) {
	f := newFixture(t)
	f.seedTap("0.8.25", "0.8.25")
	f.release("v0.8.39")

	out, err := f.run("TAP_PUSH_TOKEN=")
	if err == nil {
		t.Fatalf("tap-bump succeeded without TAP_PUSH_TOKEN:\n%s", out)
	}
	wantContains(t, "output", out, "TAP_PUSH_TOKEN is not set")
	if got := f.tapCommits(); got != 1 {
		t.Errorf("tap has %d commits, want 1 — it was pushed without a token", got)
	}
}

func TestDryRunDoesNotPush(t *testing.T) {
	f := newFixture(t)
	f.seedTap("0.8.25", "0.8.25")
	f.release("v0.8.39")

	out, err := f.run("DRY_RUN=1", "TAP_PUSH_TOKEN=")
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	wantContains(t, "output", out, "bump formula + cask to v0.8.39")
	if got := f.tapCommits(); got != 1 {
		t.Errorf("tap has %d commits after a dry run, want 1", got)
	}
}

func TestFailsWhenNothingIsInstallable(t *testing.T) {
	f := newFixture(t)
	f.seedTap("0.8.25", "0.8.25")
	f.mirrorTag("v0.8.40")

	out, err := f.run()
	if err == nil {
		t.Fatalf("tap-bump succeeded with no installable release:\n%s", out)
	}
	wantContains(t, "output", out, "has both tap assets")
}
