#!/usr/bin/env bash
#
# tap-bump.sh — converge the Homebrew tap on the newest installable release.
#
# A reconciler, not a step in a chain. It reads two pieces of state — which
# release tags on the GitHub mirror have BOTH tap assets published, and which
# version stump.wtf/homebrew-tap pins — and makes the second match the first.
# It needs no event, no dispatch and no knowledge of which run tagged what, so
# it can run on a timer, after a release, or by hand, and a missed run is
# repaired by the next one instead of being lost.
#
# That shape is the fix, not a refactor. As a job it only ever ran when the
# hop before it fired, and every way that failed was silent: the tag-push event
# pipeline.yaml waited on is never delivered for a tag pushed from inside a
# workflow, so the job did not go red, it simply never existed. The tap sat on
# v0.8.25 for five weeks while v0.8.26–v0.8.39 were built and released, and
# nothing compared the two.
#
# So this script also owns the alarm. After converging it compares the tap
# against the newest tag in the canonical checkout and fails when the tap is
# behind a tag older than STALE_AFTER_HOURS — which is what a broken mirror
# push or a dead desktop build looks like from here.
#
# Usage: scripts/tap-bump.sh
#
#   TAP_PUSH_TOKEN     Gitea PAT with write:repository on the tap. Required
#                      unless DRY_RUN=1.
#   WAIT_FOR_TAG       Poll for this tag's assets first (the post-release fast
#                      path; the desktop build takes ~8 min on the mirror).
#   WAIT_TIMEOUT       Seconds to poll for WAIT_FOR_TAG (default 1800).
#   STALE_AFTER_HOURS  Age at which an unshipped tag fails the run (default 2).
#   DRY_RUN=1          Print the diff instead of pushing.
#
# MIRROR_GIT_URL, ASSET_BASE_URL, TAP_GIT_URL and CANONICAL_REPO exist so the
# test (scripts/tapbump_test.go) can point the whole thing at a temp dir.
#
# @joestump 10/13/2026 - Created, replacing the tap-bump job in pipeline.yaml.

set -euo pipefail

MIRROR_GIT_URL="${MIRROR_GIT_URL:-https://github.com/stump-wtf/msgbrowse.git}"
# Assets are HEADed and fetched by download URL, never through the GitHub API:
# download URLs are not subject to the 60/hr unauthenticated API rate limit,
# and a 200 means the bytes are fetchable, which is what brew needs to be true.
ASSET_BASE_URL="${ASSET_BASE_URL:-https://github.com/stump-wtf/msgbrowse/releases/download}"
TAP_GIT_URL="${TAP_GIT_URL:-https://gitea.stump.rocks/stump.wtf/homebrew-tap.git}"
CANONICAL_REPO="${CANONICAL_REPO:-$(git rev-parse --show-toplevel 2>/dev/null || true)}"
WAIT_FOR_TAG="${WAIT_FOR_TAG:-}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-1800}"
WAIT_INTERVAL="${WAIT_INTERVAL:-30}"
STALE_AFTER_HOURS="${STALE_AFTER_HOURS:-2}"
MAX_CANDIDATES="${MAX_CANDIDATES:-25}"
DRY_RUN="${DRY_RUN:-}"

FORMULA="Formula/msgbrowse.rb"
CASK="Casks/msgbrowse-desktop.rb"

log() { echo "tap-bump: $*"; }
die() { echo "::error::tap-bump: $*" >&2; exit 1; }
summary() { [ -z "${GITHUB_STEP_SUMMARY:-}" ] || echo "$*" >> "$GITHUB_STEP_SUMMARY"; }

if [ -z "$DRY_RUN" ] && [ -z "${TAP_PUSH_TOKEN:-}" ]; then
  die "TAP_PUSH_TOKEN is not set. Add a Gitea PAT with write:repository on" \
      "stump.wtf/homebrew-tap as an Actions secret."
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

src_url() { echo "$ASSET_BASE_URL/$1/msgbrowse-$1-source.tar.gz"; }
zip_url() { echo "$ASSET_BASE_URL/$1/msgbrowse-desktop_darwin_universal.zip"; }

has_assets() {
  curl -sfIL --max-time 30 -o /dev/null "$(src_url "$1")" &&
    curl -sfIL --max-time 30 -o /dev/null "$(zip_url "$1")"
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# version_gt A B — true when A sorts strictly after B.
version_gt() {
  [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1)" = "$1" ]
}

# rewrite FILE SED_EXPR — in-place edit without `sed -i`, whose argument
# differs between GNU and BSD sed; this runs on the runner and on a Mac.
rewrite() {
  sed -E "$2" "$1" > "$1.new" && mv "$1.new" "$1"
}

# Release tags on the mirror, newest first. Only a tag that reached the mirror
# can have assets, so this is the whole candidate set.
mirror_tags() {
  git ls-remote --tags --refs "$MIRROR_GIT_URL" 'v*' |
    sed -E 's|.*refs/tags/||' |
    grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' |
    sort -rV | sed -n "1,${MAX_CANDIDATES}p"
}

if [ -n "$WAIT_FOR_TAG" ]; then
  log "waiting up to ${WAIT_TIMEOUT}s for $WAIT_FOR_TAG assets"
  waited=0
  until has_assets "$WAIT_FOR_TAG"; do
    # Do not exit here: fall through and ship whatever IS installable. The
    # staleness check below is what reports this tag as unshipped.
    [ "$waited" -lt "$WAIT_TIMEOUT" ] || { log "gave up waiting for $WAIT_FOR_TAG"; break; }
    sleep "$WAIT_INTERVAL"
    waited=$((waited + WAIT_INTERVAL))
  done
fi

CANDIDATES="$(mirror_tags)" || die "could not list release tags on $MIRROR_GIT_URL."
TARGET=""
for t in $CANDIDATES; do
  if has_assets "$t"; then TARGET="$t"; break; fi
done
[ -n "$TARGET" ] ||
  die "no release on $MIRROR_GIT_URL has both tap assets (checked the newest $MAX_CANDIDATES tags)."
VER="${TARGET#v}"
log "newest installable release: $TARGET"

tap_version=""
retry=""

# One attempt = clone, rewrite, verify, push. Retried from a fresh clone
# because two things can move under it: the tap (another run pushed first) and
# the assets (a second Desktop run replaced them after the checksum, which is
# how the v0.8.25 cask shipped a sha256 that matched nothing, #490).
#
# Asks for a retry by setting $retry, not by returning non-zero: bash ignores
# `set -e` inside a function called as an `if`/`||` condition, and a failed
# clone or download in here has to stop the script, not fall through.
converge() {
  local tap="$WORK/tap" cur_formula cur_cask sha_src sha_zip
  retry=""
  rm -rf "$tap"
  git clone -q --depth 1 "$TAP_GIT_URL" "$tap"

  cur_formula="$(sed -n -E 's|.*releases/download/v([0-9.]+)/msgbrowse-v[0-9.]+-source\.tar\.gz.*|\1|p' "$tap/$FORMULA")"
  cur_cask="$(sed -n -E 's|^  version "([0-9.]+)"|\1|p' "$tap/$CASK")"
  if [ -z "$cur_formula" ] || [ -z "$cur_cask" ]; then
    die "could not read the pinned version from $FORMULA / $CASK — the tap files changed shape."
  fi

  if [ "$cur_formula" = "$VER" ] && [ "$cur_cask" = "$VER" ]; then
    log "tap already pins v$VER — nothing to do"
    tap_version="$VER"
    return 0
  fi
  # Never move the tap backwards. A tap ahead of every installable release is
  # somebody's deliberate hand-bump, not something to "repair".
  if version_gt "$cur_formula" "$VER" || version_gt "$cur_cask" "$VER"; then
    echo "::warning::tap-bump: tap pins formula v$cur_formula / cask v$cur_cask," \
         "ahead of the newest installable release $TARGET — leaving it alone."
    tap_version="$cur_cask"
    return 0
  fi

  log "bumping formula v$cur_formula / cask v$cur_cask -> v$VER"
  curl -sfL --retry 3 --retry-all-errors -o "$WORK/src.tar.gz" "$(src_url "$TARGET")"
  curl -sfL --retry 3 --retry-all-errors -o "$WORK/desktop.zip" "$(zip_url "$TARGET")"
  sha_src="$(sha256 "$WORK/src.tar.gz")"
  sha_zip="$(sha256 "$WORK/desktop.zip")"

  # The formula pins an explicit tag in its url; the cask interpolates
  # v#{version}, so rewriting its `version` line moves the url with it.
  rewrite "$tap/$FORMULA" "s|releases/download/v[0-9.]+/msgbrowse-v[0-9.]+-source\.tar\.gz|releases/download/${TARGET}/msgbrowse-${TARGET}-source.tar.gz|"
  rewrite "$tap/$FORMULA" "s|^(  sha256 )\"[0-9a-f]{64}\"|\1\"${sha_src}\"|"
  rewrite "$tap/$CASK" "s|^(  version )\"[0-9.]+\"|\1\"${VER}\"|"
  rewrite "$tap/$CASK" "s|^(  sha256 )\"[0-9a-f]{64}\"|\1\"${sha_zip}\"|"

  # Assert every rewrite landed. sed exits 0 when a pattern does not match, so
  # without this a drifted formula would commit unchanged.
  if ! { grep -q "releases/download/${TARGET}/msgbrowse-${TARGET}-source.tar.gz" "$tap/$FORMULA" &&
         grep -q "sha256 \"${sha_src}\"" "$tap/$FORMULA" &&
         grep -q "version \"${VER}\"" "$tap/$CASK" &&
         grep -q "sha256 \"${sha_zip}\"" "$tap/$CASK"; }; then
    die "a rewrite did not land in $FORMULA / $CASK — the tap files changed shape."
  fi

  git -C "$tap" -c user.name="joestump-agent" \
    -c user.email="joestump-agent@noreply.stump.rocks" \
    commit -aqm "toil(msgbrowse): bump formula + cask to v${VER} (automated tap bump)"

  if [ -n "$DRY_RUN" ]; then
    git -C "$tap" show --format='%s' HEAD
    log "DRY_RUN — not pushing"
    tap_version="$VER"
    return 0
  fi

  # Re-fetch both assets and compare before publishing the pins.
  if [ "$(curl -sfL --retry 3 --retry-all-errors "$(src_url "$TARGET")" | sha256 /dev/stdin)" != "$sha_src" ] ||
     [ "$(curl -sfL --retry 3 --retry-all-errors "$(zip_url "$TARGET")" | sha256 /dev/stdin)" != "$sha_zip" ]; then
    log "$TARGET assets changed while checksumming — retrying"
    retry=1
    return 0
  fi

  if ! git -C "$tap" -c "http.extraheader=Authorization: token ${TAP_PUSH_TOKEN}" \
      push -q origin HEAD:main; then
    log "push to the tap was rejected — retrying from a fresh clone"
    retry=1
    return 0
  fi
  log "tap bumped to v$VER"
  summary "tap bumped to v${VER}"
  tap_version="$VER"
}

for attempt in 1 2 3; do
  converge
  [ -n "$retry" ] || break
  [ "$attempt" -lt 3 ] || die "could not converge the tap on $TARGET after 3 attempts."
done

# The fast path promised a specific release. Shipping an older one instead is
# still progress, but it is not success.
if [ -n "$WAIT_FOR_TAG" ] && version_gt "${WAIT_FOR_TAG#v}" "$tap_version"; then
  die "$WAIT_FOR_TAG never became installable (waited ${WAIT_TIMEOUT}s); the tap" \
      "pins v${tap_version}. Check the Desktop workflow on the mirror."
fi

# The alarm. Everything above can succeed while the train is broken upstream of
# it: no tag reaching the mirror, or no desktop build, leaves "newest
# installable" frozen and the tap faithfully converged on something old.
if [ -n "$CANONICAL_REPO" ] && git -C "$CANONICAL_REPO" rev-parse --git-dir >/dev/null 2>&1; then
  latest="$(git -C "$CANONICAL_REPO" tag -l 'v*' --sort=-v:refname | head -1)"
  if [ -n "$latest" ] && version_gt "${latest#v}" "$tap_version"; then
    tagged="$(git -C "$CANONICAL_REPO" for-each-ref --format='%(creatordate:unix)' "refs/tags/$latest")"
    age_h=$(( ($(date +%s) - tagged) / 3600 ))
    if [ "$age_h" -ge "$STALE_AFTER_HOURS" ]; then
      summary "tap is STALE: pins v${tap_version}, newest tag is ${latest} (${age_h}h old)"
      die "the tap pins v${tap_version} but ${latest} was tagged ${age_h}h ago and" \
          "has no installable release (newest with assets: ${TARGET}). The tag" \
          "never reached the GitHub mirror, or its Desktop build never" \
          "published — check the Auto release run and the mirror's Desktop workflow."
    fi
    log "$latest is ${age_h}h old and not yet installable — within the ${STALE_AFTER_HOURS}h grace"
  fi
fi
