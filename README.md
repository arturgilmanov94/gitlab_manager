# mr-review

Локальный dashboard для работы над GitLab merge requests и задачами вместе с coding-агентами
(Claude Code, Codex, Cursor), который использует **правила и review-skill самого проекта**, а не свои.
Один статический бинарник для Linux: без Python, без venv, без установки зависимостей.

<p>
<img alt="Go" src="https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white">
<img alt="Linux" src="https://img.shields.io/badge/Linux-x86--64-black?logo=linux&logoColor=white">
<img alt="License" src="https://img.shields.io/badge/license-MIT-green">
</p>

## Содержание

- [Идея](#идея)
- [Быстрый старт](#быстрый-старт)
- [Что умеет](#что-умеет)
- [Как это работает](#как-это-работает)
- [Команды](#команды)
- [Настройки](#настройки)
- [Безопасность относительно проекта](#безопасность-относительно-проекта)
- [Устройство](#устройство)
- [Сборка из исходников и релизы](#сборка-из-исходников-и-релизы)
- [Решение проблем](#решение-проблем)
- [История изменений](#история-изменений)

## Идея

В проекте уже есть всё, что нужно для ревью: `CLAUDE.md` с правилами, агент `.claude/agents/mr-review.md`,
skills, соглашения о ветках и коммитах. Этот инструмент не дублирует их, а даёт удобный интерфейс поверх:

- каждый запуск агента стартует **из корня проекта** (`cwd = PROJECT_ROOT`), поэтому Claude Code загружает
  `CLAUDE.md`, агентов, skills и settings проекта ровно как в интерактивной сессии;
- review-skill **обнаруживается, а не копируется**: после `git pull` проекта поведение ревью меняется само;
- GitLab — только через уже авторизованный `glab`; токены не хранятся и не переносятся между машинами;
- всё, что агент меняет в коде, происходит в **отдельных git worktree** внутри `runtime/`; ваша рабочая
  копия и текущая ветка никогда не трогаются;
- результаты (findings по head SHA, проверки исправлений, планы, отчёты, логи, вопросы агенту) лежат в
  локальной SQLite рядом с бинарником.

## Быстрый старт

Готовый архив для Linux x86-64 берётся из [Releases](https://github.com/arturgilmanov94/gitlab_manager/releases)
(файл `mr-review-<version>-linux-amd64.tar.gz` и `.sha256` рядом).

Предпосылки на машине: Linux x86-64, `git`, авторизованные `glab` и хотя бы один агент (`claude`, `codex`
или `cursor-agent`), и рабочая копия проекта с его `.claude/`.

```sh
# один раз на новой машине
glab auth login --hostname gitlab.example.com
claude            # залогиниться и выйти

# распаковать рядом с проектом
cd ~/projects                                    # здесь лежит tradernet/
tar -xzf mr-review-0.2.0-linux-amd64.tar.gz      # появится ~/projects/mr-review/

cd mr-review
./mr-review doctor                               # всё ли найдено
./mr-review                                      # сервер + браузер: http://127.0.0.1:8765
```

Двойной клик по `mr-review` в файловом менеджере делает то же самое. Ярлык в меню приложений:
`./install-desktop.sh` (пишет только `~/.local/share/applications/mr-review.desktop`).

Если dashboard лежит не рядом с проектом и не внутри него:

```sh
PROJECT_ROOT=/path/to/project ./mr-review doctor
# или в .env рядом с бинарником:  PROJECT_ROOT=../tradernet
```

Doctor на исправной машине выглядит так:

```
Project root           OK    /home/me/projects/tradernet  (via sibling)
Git repository         OK
Project instructions   OK    .claude/CLAUDE.md, AGENTS.md
Project Claude config  OK    .claude/  (agents: 1, skills: 2)
Review skill           OK    agent:mr-review  (.claude/agents/mr-review.md; run as `claude --agent mr-review`)
GitLab project         OK    tn/core/tradernet @ gitlab.example.com
Worktree directory     OK    /home/me/projects/mr-review/runtime/worktrees  (branches start from origin/develop)
Agent: claude          OK    /home/me/.local/bin/claude  2.1.263 (Claude Code)
Agent: codex           WARN  not found in PATH
Agent: cursor          WARN  not found in PATH
GitLab CLI             OK    /home/me/.local/bin/glab  glab 1.96.0
GitLab access          OK    Logged in to gitlab.example.com as me
Database               OK    /home/me/projects/mr-review/data/reviews.sqlite  (migrations: 1)
Logs                   OK    /home/me/projects/mr-review/logs
Server                 OK    http://127.0.0.1:8765

Ready to run.
```

## Что умеет

### Merge requests

| Кнопка | Что происходит | Что не происходит |
|---|---|---|
| **Синхронизировать с GitLab** | Подтягивает открытые MR, где вы reviewer или assignee (`GITLAB_SYNC_ROLES`), только текущего проекта. Для каждого считает нерешённые обсуждения. | Ничего не запускает. |
| **Добавить** | Загружает MR по ссылке или `!123`. | — |
| **Быстрое ревью** | Лёгкий проход: diff и нерешённые обсуждения, соседний код только когда без него не понять правку. Только CRITICAL/HIGH/MEDIUM, короткое резюме. Обычно 1–3 минуты. | Не меняет файлы, не пишет в GitLab. |
| **Полное ревью** | Полный проход агентом `mr-review` по правилам проекта: контекст затронутого кода, регрессии, обработка ошибок, безопасность, паттерны проекта. Все severity, предложения фиксов, оценка обсуждений. Обычно 3–10 минут. | То же. |
| **Проверить исправления** | После новых коммитов: каждое открытое замечание последнего ревью получает статус fixed / open / obsolete с обоснованием, плюс только новые проблемы из новых коммитов. | То же. |
| **Исправить замечания ревьюеров** | Только для *ваших* MR с нерешёнными обсуждениями. Worktree на ветке MR, агент читает комментарии через `glab`, правит код, гоняет проверки. Дальше вы смотрите diff и сами делаете Commit и Push (push обновляет MR). | Не резолвит и не комментирует обсуждения, не пушит сам. |
| **Вопрос агенту** | Продолжение той же сессии агента по результату: уточнить, оспорить, попросить расписать фикс. | Read-only. |

Список показывает head SHA, метку «новые коммиты», когда MR ушёл вперёд после ревью, число открытых
замечаний и статус последнего запуска.

### Задачи

| Кнопка | Что происходит |
|---|---|
| **Синхронизировать / Добавить** | Задачи, где вы assignee, или любая задача по ссылке (в том числе из другого проекта GitLab: код всё равно берётся из основного репозитория). |
| **Составить план** | Read-only анализ: шаги, файлы и что в них меняется, риски, открытые вопросы, оценка размера. Поле «Указания» позволяет уточнить, как решать. |
| **Реализовать в worktree** | Новая ветка от `origin/develop` (имя по умолчанию = ссылка на задачу, например `tn/project/eu/eu#11514`, как принято в проекте) в отдельном worktree. Агент реализует задачу как есть или с вашими указаниями и пишет отчёт: изменения, что прогнал, что требует решения человека, предложенное сообщение коммита. |
| **Commit / Push / Создать MR / Удалить worktree** | Каждое действие отдельной кнопкой на странице запуска, с подтверждением. Diff и `git status` worktree показываются там же. |

### Общее

- Переключатель агента **Claude / Codex / Cursor** на каждый запуск; показываются только установленные.
  Модель специально не выбирается: за неё отвечают правила и агенты проекта.
- У каждой кнопки подробная подсказка при наведении: что произойдёт, где, что не будет тронуто.
- Состояния запусков: в очереди, выполняется (спиннер, полоса прогресса, таймер), завершён, не вышло, отменён.
  Страницы с активным запуском обновляются сами.
- У каждого запуска: полный лог (команда, промпт, stdout/stderr агента), израсходованные токены с разбивкой
  (вход, выход, чтение и запись кэша, суммарно по агенту и субагентам), длительность, JSON результата.
  Доллары не показываются: при работе по подписке важны лимиты в токенах.

## Как это работает

```
 браузер ──HTTP──▶ mr-review (Go) ──▶ SQLite  data/reviews.sqlite
                       │
                       ├─▶ glab api …              метаданные MR/задач, обсуждения (read-only, кроме «Создать MR»)
                       │
                       └─▶ claude -p --agent mr-review --json-schema …   (или codex exec / cursor-agent)
                             cwd = PROJECT_ROOT            ревью, проверка, план: read-only набор инструментов
                             cwd = runtime/worktrees/<ветка>  реализация, исправление замечаний: правки только там
```

1. **Определение проекта.** `PROJECT_ROOT` → git toplevel от каталога бинарника → от текущего каталога →
   родительские каталоги → единственный соседний репозиторий с инструкциями для агентов (`CLAUDE.md`,
   `AGENTS.md`, `.claude/CLAUDE.md` или `.claude/{agents,commands,skills,rules}`). При нескольких кандидатах
   doctor перечислит их готовыми командами.
2. **Обнаружение review-skill.** Сканируются `.claude/agents/*.md` (запуск `claude --agent <name>`),
   `.claude/commands/**/*.md` и `.claude/skills/*/SKILL.md` (slash-команда в промпте). Берётся `mr-review`
   (`REVIEW_SKILL=` переопределяет), иначе первая запись, чьё имя/описание упоминают ревью merge request.
   Хранится только идентификатор вида `agent:mr-review` и относительный путь; файл валидируется при каждом doctor.
   Без skill ревью работает по `CLAUDE.md`/`AGENTS.md` плюс минимальный промпт dashboard.
3. **Промпт dashboard** добавляет только то, что нужно интерфейсу: ссылку и head SHA, режим, требование
   read-only (или правила работы в worktree) и JSON-схему результата. Правила проекта не дублируются.
4. **Структурированный результат.** Claude Code вызывается с `--json-schema`; findings (severity, файл, строка,
   описание, предложение), вердикт, нерешённые обсуждения раскладываются по таблицам. Для проверки исправлений
   прежние findings копируются в новый запуск с новыми статусами и ссылкой на оригинал.
5. **Worktree.** `git worktree add` от `origin/<BASE_BRANCH>` (или существующая ветка, локальная либо на origin)
   в `runtime/worktrees/`. `.claude/`, `CLAUDE.md`, `AGENTS.md` линкуются из основного чекаута, если проект
   держит их вне git, чтобы правила действовали и там.
6. **Продолжение сессии.** Session id агента сохраняется; «Вопрос агенту» вызывает `--resume` в том же каталоге.

## Команды

```sh
./mr-review              # сервер + браузер (для двойного клика)
./mr-review serve        # сервер в foreground без браузера
./mr-review start        # сервер в фоне (pid в runtime/server.pid, лог в logs/server.log)
./mr-review stop | status
./mr-review doctor       # проверка окружения; exit code 1 при FAIL
./mr-review init-db      # создать/обновить схему SQLite (делается и при старте)
./mr-review skill        # какой review-skill и какие файлы инструкций найдены в проекте
./mr-review config       # эффективные настройки
./mr-review sync         # MR + задачи
./mr-review add <url>    # MR или issue по ссылке
./mr-review review <url> [--kind quick|full|verify] [--runner claude|codex|cursor]
./mr-review list
./mr-review version
```

## Настройки

Файл `.env` рядом с бинарником (шаблон — `.env.example`), переменные окружения имеют приоритет.
Относительные пути считаются от каталога бинарника.

| Переменная | По умолчанию | Смысл |
|---|---|---|
| `PROJECT_ROOT` | auto | Рабочая копия основного проекта |
| `HOST`, `PORT`, `OPEN_BROWSER` | `127.0.0.1`, `8765`, `1` | Сервер и автозапуск браузера при запуске без аргументов |
| `DATABASE_PATH`, `LOG_DIR`, `RUNTIME_DIR`, `WORKTREE_DIR` | `./data/reviews.sqlite`, `./logs`, `./runtime`, `./runtime/worktrees` | Где хранить данные |
| `REVIEW_SKILL` | auto (`mr-review`) | Имя review-агента/команды/скилла проекта |
| `CLAUDE_BIN`, `CODEX_BIN`, `DEFAULT_RUNNER` | `claude`, `codex`, `claude` | Агенты (Cursor ищется как `cursor-agent`) |
| `CLAUDE_MAX_BUDGET_USD`, `RUN_TIMEOUT_SEC` | пусто, `1800` | Лимиты на запуск (бюджет Claude Code считает по API-прайсу, даже при подписке) |
| `CLAUDE_EXTRA_ALLOWED_TOOLS` | пусто | Дополнительные инструменты для read-only запусков, например `Bash(php *)` |
| `GLAB_BIN`, `GITLAB_HOST`, `GITLAB_PROJECT` | `glab`, из `git remote`, из `git remote` | GitLab; host/project задаются вручную, если remote не разбирается |
| `GITLAB_SYNC_ROLES`, `GITLAB_SYNC_ONLY_PROJECT` | `reviewer,assignee,author`, `1` | Что синхронизировать (author = мои MR) |
| `BASE_BRANCH` | `develop` | От чего создаются ветки задач |
| `RUN_CONCURRENCY` | `1` | Сколько запусков агентов параллельно |

## Безопасность относительно проекта

- Ревью, проверка и план: агент получает read-only набор инструментов (`glab api`, read-only `git`, чтение
  файлов) и явный запрет на правки, checkout, reset, stash, commit, push и запись в GitLab. Попытка обойти
  (например, `glab api … > file`) отклоняется самим Claude Code.
- Реализация и исправление замечаний: правки разрешены **только в worktree**; commit, push и создание MR
  делает dashboard по вашей кнопке с подтверждением. Агенту запрещены `git commit/push/checkout/reset` и запись в GitLab.
- Инструмент не трогает файлы проекта, `CLAUDE.md`, skills и settings; всё своё держит в `data/`, `logs/`, `runtime/`.
- Логи запусков содержат промпт и вывод агента; это локальные файлы. Credentials не читаются и не хранятся.

## Устройство

```
cmd/mr-review/          CLI и жизненный цикл сервера (start/stop/status, открытие браузера)
internal/config         .env и переменные окружения
internal/projectroot    определение PROJECT_ROOT
internal/skill          ReviewSkillResolver: агенты, команды, skills проекта
internal/gitlab         клиент glab, разбор ссылок и remote
internal/runner         интерфейс Runner и реализации: claude, codex, cursor
internal/prompts        добавления dashboard к промпту и JSON-схемы результатов
internal/worktree       git worktree для задач и исправлений
internal/app            сервис: синхронизация, очередь запусков, разбор результатов, действия с worktree
internal/db             SQLite (modernc.org/sqlite, без cgo), схема в schema.sql
internal/doctor         проверка окружения
internal/web            HTTP-сервер, шаблоны и статика (встроены в бинарник)
internal/testutil       фейки GitLab и агента для тестов
scripts/                build.sh, portability_test.sh, install-desktop.sh
```

Таблицы: `merge_requests`, `issues`, `runs` (все виды запусков), `findings`, `discussions`, `messages`.

## Сборка из исходников и релизы

```sh
go build ./cmd/mr-review                       # Go 1.22+
go test ./...
scripts/build.sh                               # dist/mr-review + dist/mr-review-<version>-linux-amd64.tar.gz (+ .sha256)
scripts/portability_test.sh dist/mr-review-<version>-linux-amd64.tar.gz /path/to/project
```

Релиз: поднять `VERSION`, дописать раздел в `CHANGELOG.md`, закоммитить, поставить тег `v<version>` и запушить —
GitHub Actions (`.github/workflows/ci.yml`) прогонит тесты, соберёт архив и приложит его к GitHub Release
с заметками из changelog.

Зависимости: стандартная библиотека и `modernc.org/sqlite`. Runner'ы Codex и Cursor реализованы по документации
CLI и включаются только при наличии бинарника в PATH; проверены только на уровне unit-тестов.

## Решение проблем

| Симптом | Что делать |
|---|---|
| `… is already served by another mr-review instance` | Запущена другая копия. Остановите её там (`./mr-review stop`) или выберите порт: `PORT=8766 ./mr-review`. |
| `Project root FAIL: Several sibling git repositories…` | Рядом несколько проектов с правилами для агентов. Укажите нужный: `PROJECT_ROOT=../tradernet` в `.env`. |
| `Review skill WARN: none detected` | В чекауте нет `.claude/agents/mr-review.md` (в проекте `.claude/` часто вне git). Синхронизируйте `.claude/`; ревью пока идёт по `CLAUDE.md`. |
| `GitLab access FAIL` | `glab auth login --hostname <host>` на этой машине. |
| Запуск завис или слишком дорог | Кнопка «Отменить»; лимиты `RUN_TIMEOUT_SEC`, `CLAUDE_MAX_BUDGET_USD`. |
| Хочу посмотреть, что именно получил агент | Страница запуска → «Промпт» и «Полный лог». |

## История изменений

См. [CHANGELOG.md](CHANGELOG.md).

## Лицензия

MIT — см. [LICENSE](LICENSE).
