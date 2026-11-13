#!/usr/bin/env bash
# «Обновить и перезапустить» from the dock / application menu (source checkout only): fast-forward to the latest
# origin/main, rebuild with the release flags, swap the binary and restart the background server. Asks first when
# a restart would cut short running agents. Progress goes to desktop notifications and logs/update.log.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1
mkdir -p logs
exec >>logs/update.log 2>&1
echo "=== $(date '+%F %T') update"

say() { echo "$*"; command -v notify-send >/dev/null && notify-send -a "MR Review" -i "$ROOT/icon.svg" "MR Review" "$*" || true; }
die() { say "Обновление не удалось: $*"; exit 1; }

[ -d .git ] || die "это не git-checkout; скачайте новый релиз вручную"
GO="$(command -v go || true)"
for g in "$HOME/.local/go/bin/go" /usr/local/go/bin/go; do [ -z "$GO" ] && [ -x "$g" ] && GO="$g"; done
[ -n "$GO" ] || die "не найден Go (ни в PATH, ни в ~/.local/go)"

git fetch -q --tags origin || die "git fetch не прошёл (сеть / доступ к origin)"
branch="$(git rev-parse --abbrev-ref HEAD)"
[ "$branch" = main ] || die "в checkout ветка $branch, а не main"
git diff --quiet && git diff --cached --quiet || die "в checkout есть незакоммиченные изменения"
before="$(cat VERSION)"
git merge -q --ff-only origin/main || die "main разошлась с origin/main, нужен ручной merge"
after="$(cat VERSION)"

CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-s -w -X main.version=$after" -o mr-review.new ./cmd/mr-review || die "сборка упала, см. logs/update.log"

if ! n="$(./mr-review.new busy)"; then
    if ! command -v zenity >/dev/null || ! zenity --question --title "MR Review" --width 360 \
        --text "Сейчас идут запуски агентов: $n. Перезапуск их прервёт.\n\nПерезапустить всё равно?" --ok-label "Перезапустить" --cancel-label "Отмена"; then
        say "Собрана $after, перезапуск отложен: идут запуски ($n). Новая версия лежит в mr-review.new."
        exit 0
    fi
fi

[ -x mr-review ] && ./mr-review stop
[ -x mr-review ] && mv -f mr-review mr-review.old
mv -f mr-review.new mr-review
./mr-review start || die "новая версия не стартовала, см. logs/server.log (старая — mr-review.old)"
if [ "$before" = "$after" ]; then say "Перезапущено, версия $after (новее нет)"; else say "Обновлено $before → $after и перезапущено"; fi
