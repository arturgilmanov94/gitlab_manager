#!/usr/bin/env bash
# Optional: register a launcher so mr-review can be started from the application menu / by double-click.
# Writes ~/.local/share/applications/mr-review.desktop pointing at the binary next to this script. Nothing else is touched.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$HERE/mr-review"
[ -x "$BIN" ] || BIN="$HERE/dist/mr-review"
[ -x "$BIN" ] || { echo "mr-review binary not found next to this script" >&2; exit 1; }
TEMPLATE="$HERE/mr-review.desktop.template"
[ -f "$TEMPLATE" ] || TEMPLATE="$HERE/scripts/mr-review.desktop.template"
ICON="$HERE/icon.svg"
[ -f "$ICON" ] || ICON="utilities-terminal"
TARGET="${XDG_DATA_HOME:-$HOME/.local/share}/applications/mr-review.desktop"
mkdir -p "$(dirname "$TARGET")"
sed -e "s|__BIN__|$BIN|" -e "s|__ICON__|$ICON|" "$TEMPLATE" > "$TARGET"
chmod +x "$TARGET"
command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$(dirname "$TARGET")" || true
echo "Installed $TARGET"
echo "Look for 'MR Review' in your application menu. Double-clicking $BIN in a file manager works too (it opens the browser)."
