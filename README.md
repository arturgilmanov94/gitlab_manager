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
- **каждое действие ищет свой skill в проекте**: ревью → `mr-review`, быстрое ревью → `mr-review-quick`,
  проверка изменений → `mr-review-verify`, исправление замечаний → `mr-fix-comments`, исследование задачи →
  `task-plan`, решение задачи → `task-implement`. Skill **обнаруживается, а не копируется**: после `git pull`
  проекта поведение меняется само. Полная карта и контракт для авторов skill — [docs/SKILLS.md](docs/SKILLS.md);
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
tar -xzf mr-review-0.7.0-linux-amd64.tar.gz      # появится ~/projects/mr-review/

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
Skill: review_full     OK    agent:mr-review  (.claude/agents/mr-review.md; run as `claude --agent mr-review`)
Skill: review_quick    WARN  mr-review-quick not found; using the review_full skill agent:mr-review (...)
Skill: review_verify   WARN  mr-review-verify not found; using the review_full skill agent:mr-review (...)
Skill: fix_comments    WARN  mr-fix-comments not found; runs use CLAUDE.md / AGENTS.md + dashboard prompt
Skill: plan            WARN  task-plan not found; runs use CLAUDE.md / AGENTS.md + dashboard prompt
Skill: implement       WARN  task-implement not found; runs use CLAUDE.md / AGENTS.md + dashboard prompt
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

WARN у `Skill:` не мешает работать: действие без своего skill идёт по правилам `CLAUDE.md` и промпту dashboard
(быстрое ревью и проверка изменений берут skill полного ревью). Чтобы действие работало «как ваш агент»,
опишите в проекте skill с ожидаемым именем — что он должен делать, написано в `docs/SKILLS.md` и в самой строке doctor.

## С чего начать

1. **Обзор** (домашняя страница) — «Нужно от меня» с одним действием на пункт: ответить агенту, разобрать
   упавший pipeline своего MR, проверить изменения после ревью, повторить упавший запуск, подготовить MR по
   готовой задаче, запустить ревью нового MR. Рядом активные агенты и счётчики моих MR.
2. **Merge requests** — синхронизируйте список; у каждого MR состояние GitLab (pipeline, approvals, отставание,
   Draft) и состояние AI-ревью с одним главным действием: «Запустить ревью» → «Открыть замечания» → после новых
   коммитов «Проверить изменения». Остальное в меню ⋯. Запуск не уводит со страницы: можно запустить несколько подряд.
3. **Задачи** — «Решить задачу» отдаёт задачу агенту целиком в отдельном workspace; «Исследовать» даёт план
   без изменений кода, после чего появляется «Реализовать этот план» и «Сохранить план в проект».
4. **Сессии** и **Workspaces** — все запуски агентов и все worktree с их состоянием на отдельных страницах.
   **История** — MR, которые вас больше не касаются (одобрены вами, влиты, закрыты, вы сняты с ревью), но по
   которым были запуски; действия те же. MR без запусков и без вашей роли при синхронизации убираются совсем,
   добавленные вручную остаются до ручного удаления.
5. **Сессия** — каждая операция агента открывается на своей странице: состояние и текущий шаг, результат в
   markdown со ссылками на код в GitLab, запросы разрешений, workspace с diff, продолжение диалога, технический лог.

Знаки состояний одинаковы везде: ✓ актуально/готово · ⚠ требует внимания · ✕ ошибка · ● выполняется · ○ ещё не было.

## Что умеет

### Merge requests

| Кнопка | Что происходит | Что не происходит |
|---|---|---|
| **Синхронизировать с GitLab** | Подтягивает открытые MR, где вы reviewer, assignee или автор (`GITLAB_SYNC_ROLES`), только текущего проекта; для каждого — обсуждения, approvals, pipeline, отставание, ваши роли. MR, которые вас больше не касаются, уходят в «Историю» (если были запуски) или убираются. | Ничего не запускает. |
| **Добавить** | Загружает MR по ссылке или `!123`. | — |
| **Запустить ревью** (главное действие, пока MR не проверен) | Полный проход агентом `mr-review` по правилам проекта: контекст затронутого кода, регрессии, обработка ошибок, безопасность, паттерны проекта. Все severity, предложения фиксов, оценка обсуждений. Обычно 3–10 минут. | Не меняет файлы, не пишет в GitLab. |
| **Проверить изменения** (главное действие, когда после ревью появились коммиты) | Каждое открытое замечание получает статус исправлено / открыто / неактуально с обоснованием, плюс только новые проблемы из новых коммитов. | То же. |
| **Быстрое ревью** (в меню ⋯) | Лёгкий проход: diff и нерешённые обсуждения, соседний код только когда без него не понять правку. Только CRITICAL/HIGH/MEDIUM. Быстрее и дешевле по токенам. | То же. |
| **Полное ревью заново** (в меню ⋯) | То же, что «Запустить ревью», для уже проверенного MR. | То же. |
| **Исправить замечания ревьюеров** | Только для *ваших* MR с нерешёнными обсуждениями. Worktree на ветке MR, агент читает комментарии через `glab`, правит код, гоняет проверки. Дальше вы смотрите diff и сами делаете Commit и Push (push обновляет MR). | Не резолвит и не комментирует обсуждения, не пушит сам. |
| **Вопрос агенту** | Продолжение той же сессии агента по результату: уточнить, оспорить, попросить расписать фикс. | Read-only. |

Список показывает состояние GitLab (pipeline, approvals, нерешённые обсуждения, отставание от целевой ветки,
Draft), метку «новые коммиты», когда MR ушёл вперёд после ревью, число открытых замечаний и статус последнего
запуска. У каждого замечания в ⋯: «Код ↗» (файл в GitLab на проверенном коммите), «Ложное срабатывание»,
«Игнорировать», «Отметить решённым» — такие замечания не считаются открытыми и не идут в проверку изменений.

### Задачи

| Кнопка | Что происходит |
|---|---|
| **Синхронизировать / Добавить** | Задачи, где вы assignee, или любая задача по ссылке (в том числе из другого проекта GitLab: код всё равно берётся из основного репозитория). |
| **Исследовать** | Read-only анализ: шаги, файлы и что в них меняется, риски, открытые вопросы, оценка размера. Поле «Указания» позволяет уточнить, как решать. После исследования главное действие — **Реализовать этот план**: план передаётся агенту автоматически. **Сохранить план в проект** выгружает его в markdown (`PLANS_DIR`, по умолчанию `<проект>/.claude/plans`), чтобы продолжить из терминала или IDE. |
| **Решить задачу** | Новая ветка от `origin/develop` (имя по умолчанию = ссылка на задачу, например `tn/project/eu/eu#11514`, как принято в проекте) в отдельном workspace. Агент реализует задачу как есть или с вашими указаниями и пишет отчёт: изменения, что прогнал, что требует решения человека, предложенное сообщение коммита. |
| **Commit / Push / Создать MR / Удалить worktree** | Каждое действие отдельной кнопкой на странице запуска, с подтверждением. Diff и `git status` worktree показываются там же. |

### Общее

- Переключатель агента **Claude / Codex / Cursor** на каждый запуск; показываются только установленные.
  Модель специально не выбирается: за неё отвечают правила и агенты проекта.
- Параллельная работа: `RUN_CONCURRENCY` агентов одновременно по **разным** MR и задачам; по одному и тому же
  объекту второй запуск отклоняется, пока активен первый. Каждый запуск в своём каталоге (корень проекта или
  собственный worktree) и со своим логом.
- У каждой кнопки подсказка при наведении: что произойдёт после нажатия. Недоступные действия объясняют причину.
- Полный контракт действий по состояниям: [docs/UX_ACTIONS.md](docs/UX_ACTIONS.md); аудит и план: [docs/UX_AUDIT.md](docs/UX_AUDIT.md), [docs/ROADMAP.md](docs/ROADMAP.md).
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
                       └─▶ claude -p --agent mr-review --json-schema … --permission-prompt-tool stdio   (или codex exec / cursor-agent)
                             cwd = PROJECT_ROOT            ревью, проверка, план: корень проекта read-only, без правок
                             cwd = runtime/worktrees/<ветка>  реализация, исправление замечаний: правки только там
                             stream-json: вызовы инструментов видны как прогресс, запросы разрешений ждут ответа в dashboard
```

1. **Определение проекта.** `PROJECT_ROOT` → git toplevel от каталога бинарника → от текущего каталога →
   родительские каталоги → единственный соседний репозиторий с инструкциями для агентов (`CLAUDE.md`,
   `AGENTS.md`, `.claude/CLAUDE.md` или `.claude/{agents,commands,skills,rules}`). При нескольких кандидатах
   doctor перечислит их готовыми командами.
2. **Обнаружение skill проекта для каждого действия.** Сканируются `.claude/agents/*.md` (запуск
   `claude --agent <name>`), `.claude/commands/**/*.md` и `.claude/skills/*/SKILL.md` (slash-команда первой
   строкой промпта). Для каждого действия ищется своё имя (`mr-review`, `mr-review-quick`, `mr-review-verify`,
   `mr-fix-comments`, `task-plan`, `task-implement`; переопределяется `SKILL_*` в `.env`). Быстрое ревью и
   проверка изменений без своего skill берут skill полного ревью; для полного ревью без точного имени подходит
   первая запись, чьё имя/описание упоминают ревью merge request. Хранится только идентификатор вида
   `agent:mr-review`; файл валидируется при каждом doctor. Без skill действие работает по `CLAUDE.md`/`AGENTS.md`
   плюс минимальный промпт dashboard. Карта и контракт: [docs/SKILLS.md](docs/SKILLS.md).
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
./mr-review skill        # карта «действие → skill проекта» и найденные файлы инструкций
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
| `SKILL_REVIEW_FULL`, `SKILL_REVIEW_QUICK`, `SKILL_REVIEW_VERIFY`, `SKILL_FIX_COMMENTS`, `SKILL_PLAN`, `SKILL_IMPLEMENT` | `mr-review`, `mr-review-quick`, `mr-review-verify`, `mr-fix-comments`, `task-plan`, `task-implement` | Имена агентов/команд/skills проекта на каждое действие ([docs/SKILLS.md](docs/SKILLS.md)); `REVIEW_SKILL` — старый алиас `SKILL_REVIEW_FULL` |
| `CLAUDE_BIN`, `CODEX_BIN`, `DEFAULT_RUNNER` | `claude`, `codex`, `claude` | Агенты (Cursor ищется как `cursor-agent`) |
| `CLAUDE_MAX_BUDGET_USD`, `RUN_TIMEOUT_SEC` | пусто, `1800` | Лимиты на запуск (бюджет Claude Code считает по API-прайсу, даже при подписке) |
| `CLAUDE_EXTRA_ALLOWED_TOOLS` | пусто | Дополнительные инструменты для read-only запусков, например `Bash(php *)` |
| `CLAUDE_PERMISSIONS`, `APPROVAL_TIMEOUT_SEC` | `auto`, `1800` | Режим прав (`auto` — классификатор + песочница; `manual` — правила проекта, остальное спрашивается; `strict` — фиксированные списки) и сколько ждать вашего ответа |
| `PLANS_DIR` | `<проект>/.claude/plans` | Куда «Сохранить план в проект» пишет markdown |
| `GLAB_BIN`, `GITLAB_HOST`, `GITLAB_PROJECT` | `glab`, из `git remote`, из `git remote` | GitLab; host/project задаются вручную, если remote не разбирается |
| `GITLAB_SYNC_ROLES`, `GITLAB_SYNC_ONLY_PROJECT` | `reviewer,assignee,author`, `1` | Что синхронизировать (author = мои MR) |
| `BASE_BRANCH` | `develop` | От чего создаются ветки задач |
| `RUN_CONCURRENCY` | `4` | Сколько агентов работают одновременно (по разным MR/задачам; один объект — один активный запуск) |

## Разрешения: как в терминале, но в браузере

Claude Code запускается по потоковому протоколу, и всё, что в терминале вызвало бы вопрос «разрешить?»,
приходит в dashboard. Сессия переходит в состояние ⚠ **Нужен ваш ответ**, в шапке появляется бейдж, в списках
MR и задач — действие «Ответить агенту». На странице сессии видны инструмент, ввод, причина и правила,
которые предлагает агент; кнопки: **Разрешить** · **Разрешить и не спрашивать в этой сессии** · **Отклонить**
с комментарием агенту. Без ответа за `APPROVAL_TIMEOUT_SEC` запрос считается отклонённым. Запуски из
`mr-review review` спрашивают прямо в терминале.

Режимы `CLAUDE_PERMISSIONS`: `auto` (по умолчанию) — классификатор Claude Code решает большинство вызовов сам и
спрашивает только то, что не может оценить; `manual` — классический режим: действуют правила проекта, всё
остальное спрашивается; `strict` — прежние фиксированные allow/deny-списки без вопросов. В `auto` и `manual`
shell работает в песочнице Claude Code, поэтому обычные команды не спрашивают, а агенту доступны субагенты,
запись во временный каталог и сеть.

## Безопасность относительно проекта

- Ревью, проверка и план: корень проекта read-only для shell агента (песочница), инструменты правки файлов
  отключены, `git checkout/reset/stash/commit/push` и запись в GitLab запрещены явно — независимо от режима прав.
- Реализация и исправление замечаний: правки разрешены **только в worktree**; commit, push и создание MR
  делает dashboard по вашей кнопке с подтверждением. Агенту запрещены `git commit/push/checkout/reset` и запись в GitLab.
- Всё остальное, что потребовало бы подтверждения, решаете вы на странице сессии; после завершения там же
  видны все запросы с решениями и вызовы, отклонённые автоматически.
- Инструмент не трогает файлы проекта, `CLAUDE.md`, skills и settings; всё своё держит в `data/`, `logs/`, `runtime/`.
- Логи запусков содержат промпт и вывод агента; это локальные файлы. Credentials не читаются и не хранятся.

## Устройство

```
cmd/mr-review/          CLI и жизненный цикл сервера (start/stop/status, открытие браузера)
internal/config         .env и переменные окружения
internal/projectroot    определение PROJECT_ROOT
internal/skill          карта действий → skills проекта (Actions), resolver агентов/команд/skills
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
go test -tags e2e -run TestE2E -v ./internal/runner   # сквозная проверка с настоящим Claude Code (тратит токены)
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
| `Skill: <действие> WARN … not found` | В проекте нет skill с ожидаемым именем. Действие работает по `CLAUDE.md` (ревью-варианты — по skill полного ревью). Создайте `.claude/skills/<имя>/SKILL.md` или укажите своё имя в `SKILL_*` (`.env`); контракт — [docs/SKILLS.md](docs/SKILLS.md). Если `.claude/` проекта вне git — синхронизируйте его. |
| `GitLab access FAIL` | `glab auth login --hostname <host>` на этой машине. |
| Запуск завис или слишком дорог | Кнопка «Отменить»; лимиты `RUN_TIMEOUT_SEC`, `CLAUDE_MAX_BUDGET_USD`. |
| Хочу посмотреть, что именно получил агент | Страница запуска → «Промпт» и «Полный лог». |

## История изменений

См. [CHANGELOG.md](CHANGELOG.md).

## Лицензия

MIT — см. [LICENSE](LICENSE).
