#!/usr/bin/env bash
# Optional: register a launcher so mr-review can be started from the application menu or by double-click.
# Writes ~/.local/share/applications/mr-review.desktop, a copy of the icon under ~/.local/share/icons/,
# and (unless --no-desktop) a copy of the launcher on the desktop. Nothing else is touched.
#   ./install-desktop.sh              menu entry + desktop shortcut
#   ./install-desktop.sh --no-desktop menu entry only
#   ./install-desktop.sh --uninstall  remove everything this script created
set -euo pipefail

WANT_DESKTOP=1
UNINSTALL=0
for arg in "$@"; do
    case "$arg" in
        --no-desktop) WANT_DESKTOP=0 ;;
        --uninstall) UNINSTALL=1 ;;
        -h|--help) sed -n '2,7p' "${BASH_SOURCE[0]}" | sed 's/^# \?//'; exit 0 ;;
        *) echo "unknown option: $arg (try --help)" >&2; exit 2 ;;
    esac
done

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}"
TARGET="$DATA_DIR/applications/mr-review.desktop"
ICON_TARGET="$DATA_DIR/icons/hicolor/scalable/apps/mr-review.svg"

# Prints the desktop directory, or nothing when there is none (never fails: the caller runs under set -e).
desktop_dir() {
    local dir=""
    if command -v xdg-user-dir >/dev/null 2>&1; then
        dir="$(xdg-user-dir DESKTOP 2>/dev/null || true)"
    fi
    if [ -z "$dir" ] || [ ! -d "$dir" ] || [ "$dir" = "$HOME" ]; then
        dir="$HOME/Desktop"
    fi
    [ -d "$dir" ] && printf '%s\n' "$dir"
    return 0
}

if [ "$UNINSTALL" = 1 ]; then
    DESK="$(desktop_dir)"
    rm -f "$TARGET" "$ICON_TARGET" ${DESK:+"$DESK/mr-review.desktop"}
    command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$(dirname "$TARGET")" || true
    echo "Removed the MR Review launcher."
    exit 0
fi

# In the release archive the binary, the template and the icon sit next to this script; in a source
# checkout the script is in scripts/ and the binary is the one you run from the repo root (data/, logs/
# and runtime/ live next to it), falling back to a fresh dist/ build.
BIN="$HERE/mr-review"
[ -x "$BIN" ] || BIN="$HERE/../mr-review"
[ -x "$BIN" ] || BIN="$HERE/dist/mr-review"
[ -x "$BIN" ] || BIN="$HERE/../dist/mr-review"
[ -x "$BIN" ] || { echo "mr-review binary not found next to this script" >&2; exit 1; }
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"

TEMPLATE="$HERE/mr-review.desktop.template"
[ -f "$TEMPLATE" ] || TEMPLATE="$HERE/scripts/mr-review.desktop.template"
[ -f "$TEMPLATE" ] || { echo "mr-review.desktop.template not found next to this script" >&2; exit 1; }

ICON_SRC="$HERE/icon.svg"
[ -f "$ICON_SRC" ] || ICON_SRC="$HERE/../icon.svg"
if [ -f "$ICON_SRC" ]; then
    # Keep our own copy: the launcher then survives moving or deleting the unpacked archive.
    mkdir -p "$(dirname "$ICON_TARGET")"
    cp "$ICON_SRC" "$ICON_TARGET"
    ICON="$ICON_TARGET"
else
    ICON="utilities-terminal"
fi

mkdir -p "$(dirname "$TARGET")"
sed -e "s|__BIN__|$BIN|" -e "s|__ICON__|$ICON|" -e "s|__WORKDIR__|$(dirname "$BIN")|" "$TEMPLATE" > "$TARGET"
chmod +x "$TARGET"
command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$(dirname "$TARGET")" || true
echo "Installed $TARGET"

if [ "$WANT_DESKTOP" = 1 ]; then
    DESK="$(desktop_dir)"
    if [ -n "$DESK" ]; then
        cp "$TARGET" "$DESK/mr-review.desktop"
        chmod +x "$DESK/mr-review.desktop"
        # GNOME/Nautilus only shows the icon instead of the file's text once the entry is trusted.
        command -v gio >/dev/null 2>&1 && gio set "$DESK/mr-review.desktop" metadata::trusted true 2>/dev/null || true
        echo "Installed $DESK/mr-review.desktop"
    else
        echo "No desktop directory found — skipped the desktop shortcut." >&2
    fi
fi

echo "Look for 'MR Review' in your application menu. Double-clicking $BIN in a file manager works too (it opens the browser)."
