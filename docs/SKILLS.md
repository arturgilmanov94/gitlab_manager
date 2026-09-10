# Карта действий → skills проекта

Dashboard — это интерфейс поверх агентов **вашего проекта**, а не отдельный ревьюер. Каждое действие
сначала ищет в `.claude/` проекта агента, команду или skill с ожидаемым именем и отдаёт ему запуск.
Своих правил у dashboard нет: он добавляет к промпту только ссылку на MR/задачу, режим, ограничения
(read-only или «правки только в worktree») и JSON-схему результата.

Проверить, что нашлось, можно тремя способами: `./mr-review skill`, `./mr-review doctor`
(строки `Skill: <действие>`), страница «Проверка окружения» в браузере (таблица «Действия dashboard → skills проекта»).
На странице каждого запуска показан идентификатор skill, с которым он выполнялся.

## Карта

| Действие (кнопка) | `kind` | Ожидаемое имя | Переменная `.env` | Если skill нет |
|---|---|---|---|---|
| Запустить ревью / Полное ревью | `review_full` | `mr-review` | `SKILL_REVIEW_FULL` (алиас `REVIEW_SKILL`) | любой агент/skill, чьё имя или описание упоминают review + merge request; иначе CLAUDE.md + промпт dashboard |
| Быстрое ревью | `review_quick` | `mr-review-quick` | `SKILL_REVIEW_QUICK` | skill `review_full` с пометкой «быстрый проход: только diff» |
| Проверить изменения | `review_verify` | `mr-review-verify` | `SKILL_REVIEW_VERIFY` | skill `review_full` с прежними findings в промпте |
| Проверить замечание | `verify_finding` | `mr-verify-finding` | `SKILL_VERIFY_FINDING` | skill `review_full` с одним замечанием в промпте |
| Проверить на стенде | `stand_test` | `mr-stand-test` | `SKILL_STAND_TEST` | сценарий dashboard поверх skill доступа к стенду (`STAND_SKILL`, ищется по слову «стенд») |
| Исправить замечания ревьюеров | `fix_comments` | `mr-fix-comments` | `SKILL_FIX_COMMENTS` | CLAUDE.md + промпт dashboard |
| Исследовать | `plan` | `task-plan` | `SKILL_PLAN` | CLAUDE.md + промпт dashboard |
| Решить задачу / Реализовать этот план | `implement` | `task-implement` | `SKILL_IMPLEMENT` | CLAUDE.md + промпт dashboard |

## Переопределение из dashboard

На странице «Проверка окружения» в таблице действий у каждой строки есть колонка «Настройка»:

- **Skill проекта** — выпадающий список всех найденных agents/commands/skills проекта. Выбранное имя заменяет ожидаемое
  (то же, что `SKILL_*` в `.env`, но без перезапуска и с приоритетом над `.env`).
- **Использовать свои инструкции** — галка и текст (markdown). Текст хранится в базе dashboard (`data/`), в проект не
  пишется, и подставляется в промпт запуска вместо skill: агент запускается без `--agent`, блок
  «developer instructions» идёт после описания MR/задачи. Ограничения dashboard (read-only, правки только в workspace,
  JSON-схема результата) остаются. Идентификатор на странице сессии — `custom:<kind>`.

Где ищется имя (в порядке приоритета при совпадении имён):

1. `.claude/agents/<name>.md` — запускается как `claude --agent <name>`; frontmatter обязан содержать `name:`.
2. `.claude/commands/**/<name>.md` — вызывается slash-командой `/<name> <url>` первой строкой промпта.
3. `.claude/skills/<name>/SKILL.md` — так же, slash-командой `/<name> <url>`.

Имя берётся из `name:` во frontmatter, при его отсутствии — из имени файла/каталога.

## Что dashboard передаёт и что ждёт назад

Ниже — контракт для автора skill: входные данные, которые кладёт dashboard, и поля результата, которые он
разбирает. Всё остальное (как ревьюить, какие правила проекта применять, что считать критичным) — дело skill.

### `review_full` — полное ревью MR

**Вход:** ссылка на MR, project path, IID, заголовок, ветки, head SHA. Режим read-only: файлы не менять,
git-состояние не трогать, в GitLab не писать.

**Результат (JSON по схеме):** `summary`, `verdict` (`approve` | `approve_with_comments` | `request_changes` | `blocked`),
`reviewed_sha`, `findings[]` (`severity` CRITICAL…INFO, `category`, `file`, `line`, `title`, `description`, `suggestion`),
`unresolved_discussions[]` (`author`, `file`, `line`, `body`, `assessment`, `addressed`).

Пример в tradernet: `.claude/agents/mr-review.md`.

### `review_quick` — быстрое ревью

Тот же вход и результат. Ожидается лёгкий проход: diff и обсуждения, соседний код только когда без него
не понять правку, в отчёт — CRITICAL/HIGH/MEDIUM. Без своего skill dashboard берёт skill полного ревью и
добавляет в промпт «Mode: QUICK REVIEW (light pass)».

### `review_verify` — проверка изменений после ревью

**Вход:** как у полного ревью, плюс `Previously reviewed SHA` и JSON-список прежних открытых findings
(`finding_id`, `severity`, `file`, `line`, `title`, `description`).

**Результат:** `summary`, `verdict`, `reviewed_sha`, `verified[]` (`finding_id`, `status` `open` | `fixed` | `obsolete`, `note`),
`new_findings[]` (только проблемы из новых коммитов), `unresolved_discussions[]`.

### `verify_finding` — проверка одного замечания

**Вход:** как у полного ревью, плюс одно замечание в JSON (`finding_id`, `severity`, `category`, `file`, `line`, `title`,
`description`, `suggestion`). Задача — второе мнение: не доверять замечанию, перепроверить по коду на head SHA, остальной MR не ревьюить.

**Результат:** `status` (`confirmed` | `false_positive` | `obsolete` | `unclear`), `evidence` (markdown с путями и строками),
`severity` (уточнённая или пустая), `suggestion` (уточнённый фикс или пусто). Dashboard переносит итог на замечание:
ложное и неактуальное закрывают его, подтверждённое остаётся открытым с пометкой.

### `stand_test` — проверка MR на стенде

**Вход:** как у исправления замечаний (worktree на ветке MR, режим правок), плюс имя и путь skill доступа к стенду
(`STAND_SKILL`; без него — из CLAUDE.md) и указания разработчика. Ожидаемый сценарий: `git diff --name-status origin/<target>...HEAD`
→ залить эти файлы на стенд средствами skill стенда → прогнать там тесты по затронутому коду → написать в worktree скрипт-эмуляцию
функциональности MR с моками внешних систем, залить и запустить на стенде. Запреты: `git push/pull/checkout/reset` и миграции
на стенде, правки кода MR (только скрипт и фикстуры).

**Результат:** `summary`, `deployed[]`, `tests`, `script_path`, `script_output`, `problems[]` (`severity`, `title`, `description`),
`changes[]`, `todo[]`, `self_review` (обзор собственного диффа), `commit_message`. Скрипт остаётся в workspace: Commit / Push / удалить — по кнопкам на странице сессии.

### `fix_comments` — исправление замечаний ревьюеров

**Вход:** данные MR, указания разработчика. Агент уже находится в worktree на ветке MR (`cwd`), правки
разрешены только там; commit/push/запись в GitLab запрещены — это делает разработчик кнопками dashboard.

**Результат:** `summary`, `addressed[]` (`author`, `file`, `line`, `comment`, `action`, `done`), `changes[]` (`path`, `description`),
`tests`, `todo[]` (что требует решения человека), `commit_message`.

### Поле `ask` — вопросы разработчику

Схемы `plan`, `implement`, `fix_comments`, `stand_test` содержат массив `ask[]` (`question`, `options[]`, `why`). Если агент
не может продолжить без решения разработчика, он возвращает результат с заполненным `ask` и минимальными остальными полями;
dashboard переводит сессию в «Нужен ваш ответ», показывает форму и после ответов продолжает **ту же** сессию промптом
«The developer answered your questions … continue and return the full structured result». В обычном случае `ask` пустой.

### `plan` — исследование задачи

**Вход:** ссылка на задачу, project path, IID, заголовок, описание, указания разработчика. Режим read-only, `cwd` = корень проекта.

**Результат:** `summary`, `steps[]`, `files[]` (`path`, `change`), `risks[]`, `questions[]`, `estimate`.

### `plan` в режиме бага

Тот же вход, промпт «Mode: BUG ANALYSIS». Результат: `summary`, `expected`, `actual`, `reproduction[]`, `path[]`, `root_cause`,
`evidence[]`, `fix`, `risks[]`, `questions[]`, `estimate`, `ask[]`. При «Исправить» root cause, fix и evidence передаются в
указания реализации текстом.

### `implement` — решение задачи

**Вход:** как у `plan`, плюс имя ветки и базовая ветка; если запуск сделан кнопкой «Реализовать этот план»,
в указания добавлен текст плана. Агент в worktree на новой ветке; commit/push запрещены.

**Результат:** `summary`, `changes[]` (`path`, `description`), `tests`, `todo[]`, `commit_message`.

## Шаблон skill

Минимальный `.claude/skills/task-plan/SKILL.md`:

```markdown
---
name: task-plan
description: Исследование задачи GitLab перед реализацией — план, файлы, риски, вопросы.
---

Ты исследуешь задачу GitLab в этом репозитории. Ссылка и описание задачи приходят в промпте.

1. Прочитай задачу; при необходимости подтяни связанные issue/MR через `glab api` (только GET).
2. Найди затронутые модули по `.agents/rules/project-map.mdc` и описаниям модулей.
3. Составь план: шаги, файлы и что в них меняется, риски и регрессии, открытые вопросы, оценка.
4. Код не меняй. Ответ — строго JSON по схеме, которую даёт dashboard.
```

Агент вместо skill (`.claude/agents/task-implement.md`) отличается только тем, что dashboard запустит его через
`claude --agent task-implement`; frontmatter обязан содержать `name:`.

## Что доступно skill-у внутри запуска dashboard

В режиме `CLAUDE_PERMISSIONS=auto` (по умолчанию) агент работает в песочнице Claude Code: можно вызывать
субагентов (`Agent`, в том числе с указанием модели), писать во временный каталог (`$TMPDIR`), сохранять туда
дифф, ходить в сеть. В read-only действиях (ревью, проверка, исследование) корень проекта закрыт для записи,
инструменты `Edit`/`Write` отключены, `git checkout/commit/push` и запись в GitLab запрещены. Всё, что вызвало
бы вопрос в терминале, показывается разработчику в dashboard как «Нужен ваш ответ» — skill может рассчитывать
на это, но не должен требовать интерактивности: пока нет ответа, запуск стоит.

## Параллельная работа

`RUN_CONCURRENCY` в `.env` задаёт, сколько агентов работают одновременно. По разным MR и задачам запуски идут
параллельно; по одному и тому же MR/задаче второй запуск отклоняется, пока активен первый. Worktree создаются
под общим замком на `.git` основного чекаута, чтобы параллельные `git fetch`/`git worktree add` не мешали друг другу;
сам агент после этого работает в своём каталоге независимо.
