#!/usr/bin/env bash
# Build the static Linux binary into dist/ and assemble the release archive.
#   scripts/build.sh            -> dist/mr-review (linux/amd64) + dist/mr-review-<version>-linux-amd64.tar.gz
# Requires Go 1.22+ (GO env var may point at a specific go binary).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE"
GO="${GO:-go}"
VERSION="$(cat VERSION)"
ARCH="${GOARCH:-amd64}"
NAME="mr-review-${VERSION}-linux-${ARCH}"

mkdir -p dist
echo "==> building mr-review ${VERSION} (linux/${ARCH})"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" "$GO" build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o dist/mr-review ./cmd/mr-review

echo "==> scanning binary for machine-specific paths"
for needle in "$HOME" "$HERE"; do
    if strings dist/mr-review | grep -qF "$needle"; then
        echo "ERROR: binary contains local path: $needle" >&2; exit 1
    fi
done

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/mr-review"
cp dist/mr-review README.md LICENSE VERSION .env.example "$STAGE/mr-review/"
cp scripts/mr-review.desktop.template scripts/install-desktop.sh scripts/portability_test.sh "$STAGE/mr-review/"
mkdir -p "$STAGE/mr-review/scripts" && mv "$STAGE/mr-review/portability_test.sh" "$STAGE/mr-review/scripts/"
cat > "$STAGE/mr-review/MANIFEST.md" <<MANIFEST
# mr-review ${VERSION} — release manifest

- **Version:** ${VERSION}
- **Build date:** $(date -u '+%Y-%m-%d %H:%M UTC')
- **Target:** linux/${ARCH}, static binary (CGO disabled, pure-Go SQLite)
- **Go:** $("$GO" version | awk '{print $3}')

## Contents

- \`mr-review\` — the dashboard (web UI + CLI), single executable
- \`.env.example\` — all settings; copy to \`.env\` next to the binary if you need to change anything
- \`install-desktop.sh\` + \`mr-review.desktop.template\` — optional launcher for the application menu / double-click
- \`scripts/portability_test.sh\` — smoke test for a fresh unpack
- \`README.md\`, \`LICENSE\`, \`VERSION\`, \`MANIFEST.md\`

## Required on the target machine (installed/authorised separately)

- Linux x86-64, \`git\`
- Claude Code CLI (\`claude\`), logged in — and/or Codex CLI (\`codex\`) / Cursor CLI (\`cursor-agent\`)
- GitLab CLI (\`glab\`), logged in to your GitLab host
- A working copy of the main project with its Claude rules (CLAUDE.md, .claude/agents/mr-review.md, ...)

## Intentionally NOT included

- \`.env\`, credentials, GitLab or Claude tokens
- the SQLite database, logs, runtime files, worktrees (\`data/\`, \`logs/\`, \`runtime/\` are created next to the binary)
- a copy of the main project or of its review skill (discovered at runtime from PROJECT_ROOT)
- source code (see the repository; \`go build ./cmd/mr-review\` reproduces the binary)

## Checksums

SHA256 sums are written next to the archive as \`<archive>.sha256\`.
MANIFEST
tar -C "$STAGE" -czf "dist/${NAME}.tar.gz" --owner=0 --group=0 mr-review
(cd dist && sha256sum "${NAME}.tar.gz" > "${NAME}.tar.gz.sha256" && sha256sum mr-review > mr-review.sha256)
echo "==> dist/${NAME}.tar.gz"
cat "dist/${NAME}.tar.gz.sha256"
