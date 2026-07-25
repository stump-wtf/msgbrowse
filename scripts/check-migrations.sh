#!/usr/bin/env bash
#
# check-migrations.sh — enforce that shipped migrations are immutable.
#
# Issue #217: two branches independently defined a migration numbered v11 and
# both were applied to real databases. `PRAGMA user_version` is a single
# integer, so nothing in either database could say which v11 it had run. The
# runtime now records a checksum per applied migration
# (internal/store/migrations.go), which detects the damage — this script
# prevents it, by failing the build when a migration that already exists on the
# base branch has its SQL changed.
#
# The rule: migrations are append-only. To change the schema, add a new
# version. Never edit one that has already shipped, because databases in the
# wild ran the old text and no amount of renumbering afterwards can tell you
# which text they ran.
#
# Usage: scripts/check-migrations.sh [base-ref]   (default: origin/main)

set -euo pipefail

BASE_REF="${1:-origin/main}"
SCHEMA_FILE="internal/store/schema.go"

cd "$(git rev-parse --show-toplevel)"

if [[ ! -f "$SCHEMA_FILE" ]]; then
  echo "check-migrations: $SCHEMA_FILE not found" >&2
  exit 1
fi

# Split schema.go into one file per migration constant. Two forms are
# supported: a raw-string literal (the usual case) and a single-line
# expression (schemaV14 is composed from the constants it repairs).
extract_to_dir() {
  local src="$1" outdir="$2"
  mkdir -p "$outdir"
  awk -v outdir="$outdir" '
    /^const schemaV[0-9]+ = `/ {
      v = $2; sub(/^schemaV/, "", v)
      f = outdir "/" v
      printf "" > f
      inblock = 1
      next
    }
    inblock && /^`$/ { close(f); inblock = 0; next }
    inblock { print >> f; next }
    /^const schemaV[0-9]+ = / {
      v = $2; sub(/^schemaV/, "", v)
      f = outdir "/" v
      r = $0; sub(/^const schemaV[0-9]+ = /, "", r)
      print r > f
      close(f)
    }
  ' "$src"
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

extract_to_dir "$SCHEMA_FILE" "$tmp/head"

if ! git rev-parse --verify --quiet "$BASE_REF" >/dev/null; then
  echo "check-migrations: base ref '$BASE_REF' not available — SKIPPING the"
  echo "                  immutability check (only the local consistency test ran)."
  exit 0
fi

git show "$BASE_REF:$SCHEMA_FILE" > "$tmp/base-schema.go" 2>/dev/null || {
  echo "check-migrations: $SCHEMA_FILE does not exist on $BASE_REF — nothing to compare."
  exit 0
}
extract_to_dir "$tmp/base-schema.go" "$tmp/base"

violations=0
for basefile in "$tmp"/base/*; do
  [[ -e "$basefile" ]] || continue
  version="$(basename "$basefile")"
  headfile="$tmp/head/$version"

  if [[ ! -f "$headfile" ]]; then
    echo "FAIL: migration v$version exists on $BASE_REF but was DELETED."
    echo "      Databases that applied it still exist. Migrations are append-only."
    violations=$((violations + 1))
    continue
  fi

  if ! diff -q "$basefile" "$headfile" >/dev/null; then
    echo "FAIL: migration v$version was EDITED after shipping on $BASE_REF."
    echo "      Databases in the wild already ran the old text; changing it now"
    echo "      makes user_version = $version mean two different schemas (#217)."
    echo "      Add a new migration version instead. Diff:"
    diff -u "$basefile" "$headfile" | sed 's/^/        /' || true
    violations=$((violations + 1))
  fi
done

if (( violations > 0 )); then
  echo
  echo "check-migrations: $violations shipped migration(s) modified. Refusing the build."
  exit 1
fi

echo "check-migrations: OK — no shipped migration was edited or removed (base: $BASE_REF)."
