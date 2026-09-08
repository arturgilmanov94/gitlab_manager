#!/usr/bin/env bash
# Smoke test for a release archive on a clean directory.
#   scripts/portability_test.sh dist/mr-review-X.Y.Z-linux-amd64.tar.gz [PROJECT_ROOT]
set -euo pipefail
ARCHIVE="${1:?usage: portability_test.sh <archive.tar.gz> [PROJECT_ROOT]}"
ARCHIVE="$(cd "$(dirname "$ARCHIVE")" && pwd)/$(basename "$ARCHIVE")"
ROOT="${2:-}"
fail() { printf 'FAIL  %s\n' "$*" >&2; exit 1; }
pass() { printf 'PASS  %s\n' "$*"; }
step() { printf '\n--- %s\n' "$*"; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mr-review-portability.XXXXXX")"
cleanup() { [ -x "$WORK/mr-review/mr-review" ] && (cd "$WORK/mr-review" && ./mr-review stop >/dev/null 2>&1 || true); [ "${KEEP:-0}" = "1" ] || rm -rf "$WORK"; }
trap cleanup EXIT

step "Unpacking into $WORK"
tar -xzf "$ARCHIVE" -C "$WORK"
BIN="$WORK/mr-review/mr-review"
[ -x "$BIN" ] || fail "archive has no executable mr-review/mr-review"
pass "archive layout"

step "Scanning for local paths / credentials"
for f in README.md .env.example MANIFEST.md install-desktop.sh; do
    grep -qF "$HOME" "$WORK/mr-review/$f" && fail "$f contains \$HOME"
done
strings "$BIN" | grep -qF "$HOME" && fail "binary contains \$HOME"
grep -rIlE 'glpat-[A-Za-z0-9_-]{10,}|sk-ant-[A-Za-z0-9_-]{10,}' "$WORK/mr-review" && fail "token-like strings found"
for forbidden in .env data logs runtime; do [ ! -e "$WORK/mr-review/$forbidden" ] || fail "forbidden entry: $forbidden"; done
pass "no local paths, credentials or runtime files"

step "Doctor (PROJECT_ROOT=${ROOT:-auto})"
if [ -n "$ROOT" ]; then export PROJECT_ROOT="$ROOT"; fi
(cd "$WORK/mr-review" && ./mr-review doctor) || fail "doctor reported FAIL"
pass "doctor"

step "Skill resolution"
OUT="$(cd "$WORK/mr-review" && ./mr-review skill)"
echo "$OUT" | grep -q "^project root:" || fail "project root not printed"
echo "$OUT" | grep -q "review skill: agent:" || echo "NOTE: no review agent detected"
pass "project root and skill resolved from the repository"

step "Database + server"
(cd "$WORK/mr-review" && ./mr-review init-db) || fail "init-db"
[ -f "$WORK/mr-review/data/reviews.sqlite" ] || fail "database not created"
PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])' 2>/dev/null || echo 18765)"
(cd "$WORK/mr-review" && PORT="$PORT" OPEN_BROWSER=0 ./mr-review start) || fail "server did not start"
HEALTH="$(curl -sf "http://127.0.0.1:$PORT/api/health")" || fail "health endpoint"
echo "$HEALTH" | grep -q '"ok":true' || fail "health payload: $HEALTH"
curl -sf "http://127.0.0.1:$PORT/" | grep -q "MR Review" || fail "index page"
curl -sf "http://127.0.0.1:$PORT/issues" | grep -q "Задачи" || fail "issues page"
curl -sf "http://127.0.0.1:$PORT/doctor" | grep -q "Review skill" || fail "doctor page"
(cd "$WORK/mr-review" && ./mr-review status) || fail "status"
(cd "$WORK/mr-review" && ./mr-review stop)
pass "server start / health / pages / stop"

printf '\nPortability test PASSED (%s)\n' "$ARCHIVE"
