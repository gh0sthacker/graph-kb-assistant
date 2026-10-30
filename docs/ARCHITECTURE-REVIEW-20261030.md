# Архитектура Graph KB Assistant и контура охоты

Дата среза: **2026-10-30**. Назначение документа — внешнее архитектурное ревью, в том числе передача другому агенту.

**Проверенные исходники:**

- `G` = `/home/max/bb/graph-kb-assistant`, HEAD `e11547fdacc22614356f136021bcde4f70044527`; исходное рабочее дерево чистое.
- `H` = `/home/max/bb/hunter-tools`, HEAD `4ef4e2ad632f2b722c5cfdbee63e0e2e884861f7`; на момент чтения есть неотслеживаемые `ui-proxy.py`, `ui-proxy-ct.py`. Они не являются подтверждённой частью committed runtime.
- `P` = `/home/max/.hermes/profiles`; прочитаны несекретные поля конфигураций `bb-hunter`, `bb-recon` и их `SOUL.md`. Профили не изменялись.

Ссылки вида `G/internal/store/store.go:20` — файл и строка в указанном checkout, а не ссылка на старый документ. Для конфигураций вне репозитория указаны отдельные пути. Числа интерфейсов получены разбором исходников, не запуском бинарника.

**Граница достоверности.** Это статический разбор. Охота, recon, тесты, сборки и обращения к стендам, Caido, CT108, Neo4j или LLM в рамках подготовки не запускались. Наличие реализации не означает её развёртывание или успешную эксплуатационную проверку. Исторические сведения из `docs/PROJECT-STATUS.md` ниже явно датированы и не выдаются за состояние процесса сегодня. Секреты и содержимое захваченного трафика не выгружались.

## 1. Назначение и границы

### 1.1. Продукт

Graph KB Assistant — локальное хранилище и инструменты анализа **наблюдавшегося** HTTP-поведения: привязать данные к проекту Caido, принять историю/новые записи, сохранить provenance, построить структурные поисковые индексы и BehaviorGraph, дать ограниченные выборки через CLI, web UI и MCP. Отдельная KB даёт справочные материалы с идентификаторами источников, ревизиями, хешами и диапазонами текста.

Go — основной application runtime (`G/cmd/bb-assistant/main.go:112`, `G/internal/managedapp/app.go:1`). Python в `G/src/bb_assistant` не исчез: здесь остаются совместимая модель данных, миграции и обслуживание KB; retrieval-модели запускаются отдельным Python worker (`G/internal/kb/model_worker.go`, `G/src/bb_assistant/kb/cli.py`). Поэтому «продукт на Go» не означает отсутствие Python-зависимостей.

Продукт **не** является автономным сканером/эксплойтером, не превращает графовую корреляцию в доказательство уязвимости и не доказывает полноту Caido history. `serve` не начинает Caido intake и не вызывает охотника/модель (`G/internal/managedapp/app.go:1–3,96–114,183`). `gate`, в отличие от пассивного query API, сам делает ограниченные сетевые пробы; KB maintenance умеет получать источники. Эти исключения нельзя скрывать общим ярлыком «полностью read-only».

### 1.2. Контур охоты

`H` — shell/Python orchestration: подготовка scope, readiness gate, локальный policy proxy, recon, детерминированный triage, Hermes-специалисты, проверка результатов, coverage, obligations supervisor, ledger и отчёт. Активность требует `sweep.sh --execute`; без флага показывается план (`H/sweep.sh:148–174`). Разрешение на сам запуск не заменяет scope и отдельный opt-in на мутации.

Контур не является доказанно изолированной песочницей: proxy ENV не запрещает процессу делать прямой сетевой вызов; некоторые локальные адаптеры жёстко выбирают другой proxy. «38 классов» — размер taxonomy и plugin registry, **не** число классов, полностью проведённых сквозным sweep. Прямые сетевые verifiers, интерпретация приложенных fixture и browser/callback oracle — разные уровни доказательности (разделы 7–8).

## 2. Компоненты и их роли

### 2.1. Карта продукта

| Компонент | Роль и фактическая граница |
|---|---|
| `G/cmd/bb-assistant/` | Разбор 33 верхнеуровневых команд; `main.go` — dispatcher/maintenance/intake/query; `serve.go` — managed app; `gate.go`, `coverage.go`, `status.go`, `caido_sync.go` — отдельные сценарии. |
| `internal/managedapp` | Один процесс: HTTP UI + `/mcp`, по явному списку проектов — projection workers; совместное завершение при остановке компонента. До 16 project keys (`app.go:40–55,86–114,177–218`). |
| `internal/caido` | Адаптер read-source и процесс pinned Caido MCP; проверка executable SHA-256, отдельное окружение, token file, нормализация ответов/границ (`mcp_process.go:424–503`, `adapter.go`). Это не собственная база Caido. |
| `internal/ingest` | Historical import, live tail, inventory sync, receipts, vault, независимые projection jobs, reconciliation и controlled backfill. |
| `internal/store`, `internal/project`, `internal/projectlock` | Capture DB v7, реестр проектов и identity, разрешение private data root, межпроцессная maintenance lease. |
| `internal/graph` | Нормализация HTTP в entities/assertions/cards, SQLite BehaviorGraph, immutable imports, durable jobs и Neo4j outbox. |
| `internal/neo4jstore`, `internal/graphsync` | Neo4j driver/schema/read queries, project snapshot/generation и доставка outbox. Neo4j — default read backend; SQLite включается явно для совместимости (`graphaccess/reader.go:141–147`). |
| `internal/graphaccess` | Backend-neutral readers и process service, one-hop neighborhood, signatures/value-reference traversal. |
| `internal/search`, `internal/appquery` | Структурный поиск captures, surface catalog, typed graph/evidence/compare/evidence-packet use cases и cursor state. HTTP и MCP используют общую query-логику. |
| `internal/webui` | Embedded HTML/JS/CSS, HTTP router, loopback/Origin gate, redacted consultation workspace и чувствительное чтение historical evidence. |
| `internal/mcpserver` | Stdio JSON-RPC и Streamable HTTP над тем же каталогом; 16 штатных `bb_*`, ещё один sensitive raw tool по opt-in. |
| `internal/consultation` | Project-scoped sessions, operator review, proposals, citation receipts и immutable candidate versions. Это не автоматическая эксплуатация и не подтверждение impact. |
| `internal/kb` | Read-only local KB queries, SQLite FTS5 либо отдельный Neo4j Lucene, опциональный Python retrieval worker/RedAmon backend. |
| `internal/coverage`, `internal/gate` | Матрица заявленных классов/бюджетов и readiness probes; разные контракты, не vulnerability oracle. |
| `internal/publicdata`, `projectaudit`, `runtimeaudit`, `projectbackup`, `historyreconcile`, `buildinfo` | Validation/redaction, диагностика совместимости и coverage, snapshot/archive, reconciliation, version stamping. |

### 2.2. Внешние зависимости

- **Caido** принимает proxied HTTP(S) и является первичным источником истории. Исторические encrypted bytes дополнительно сохраняются продуктом, но live-tail сохраняет структурную проекцию/подписи, а не независимую полную копию raw сообщений (`G/internal/mcpserver/server.go:1270–1288`; `G/internal/ingest/live_tail.go:108–123`). Наличие proxy-запроса само по себе не доказывает попадание в нужный проект Caido.
- **Neo4j** хранит опубликованную graph projection. Устойчивые исходные checkpoints/jobs и локальный graph snapshot остаются в SQLite; это не одна распределённая транзакция. `GraphRecord`, `GraphImport`, `GraphProject` разделяются `project_key`, snapshot имеет generation (`G/internal/neo4jstore/store.go:186–195,484`).
- **KB-индекс** — отдельный корпус, не target graph. Полный manifest перечисляет `tool_docs`, `gtfobins`, `lolbas`, `owasp`, `exploitdb`, `nvd`, `nuclei` (`G/src/bb_assistant/kb/sources.yaml:5–11`). Не смешивать Neo4j KB lexical database и target graph endpoint: у них разные ENV prefixes (`G/internal/kb/neo4j_lexical.go:60`).

### 2.3. Hermes и инструментарий

| Компонент | Реальная роль |
|---|---|
| `P/bb-hunter/config.yaml` | Default model `deepseek-v4.1-flash`, provider `opencode-go`; toolsets: MCP приложения, skills, clarify, memory, MCP exploit_search. В allowlist приложения 15 инструментов: нет `bb_coverage_matrix` и `bb_get_capture_raw` (`config.yaml:1–52`). |
| `P/bb-hunter/SOUL.md` | Нормативные роли recon → анализ → verify → draft report, scope/approval/redaction. Упоминание `terminal/browser/file` в SOUL не включает их: доступность определяется config. Это заметное расхождение с поведением specialist runner (см. 8). |
| `P/bb-recon/config.yaml`, `SOUL.md` | Тот же default model/provider; toolsets terminal/browser/web/file/skills/memory/clarify. SOUL требует Caido `127.0.0.1:8080`, активный проект без переключений, synthetic credentials вне репозитория, явные gaps. Сейчас SOUL закрепляет только origin `http://192.168.1.149:3000` и запрещает fuzzing/угадывание routes (`SOUL.md:17–21`). Это не generic permission на любой target. Сам `sweep.sh` этот профиль не запускает; standalone recon runner с ffuf не следует автоматически считать реализацией ограничений bb-recon. |
| `H/runners/hunter-agent-runner` | `triage` — локальные сигналы по 38 классам и optional graph surface; `specialist` — Hermes `-p bb-hunter chat --oneshot --yolo --run-budget … --query-file …`, parsing/одна correction pass (`:21`, `:1052–1159`). |
| `H/runners/hunter-recon-runner` | Не Hermes `bb-recon`, а самостоятельный Python pipeline: Katana → HTML/JS/jsluice → ffuf/feroxbuster → OpenAPI/parameter discovery/httpx и валидация SPA-placeholder. В коде фиксирован proxy `127.0.0.1:8080`, общий deadline 180 s, до 80 HTML pages/250 JS assets, caps на body (`:28–45`, `:845–1386`). README с «30 GET / 1 MiB» устарел. |
| `H/runners/identities.py` | Специфичный для лабораторного API bootstrap двух акторов: POST `/api/Users`, POST `/rest/user/login`, `identities.json` с synthetic паролями/token, chmod 0600 (`:38–71`). Не универсальный auth broker. |
| `H/sweep.sh`, `run-sweep.sh` | Первый — orchestration и интеграционные точки; второй — локальный operator wrapper, не самостоятельная policy boundary. |
| `H/gateway/gateway.py` | HTTP policy proxy, request journal и CONNECT tunnel к Caido; не TLS MITM и не OS-level egress enforcement. |
| `H/governor.py` | Shared rate state + breaker; shims для ffuf/feroxbuster/katana, scanner serialization/pause. |
| `H/verify/verify.py`, `verify/verifiers/` | Generic replay и отдельный class dispatch; класс определяется finding, не playbook. Дополнительные plugins часто оценивают предоставленные факты, а не заново получают evidence. |
| `H/browser-runner/runner.py`, `callback/listener.py`, `upload-probe/probe.py`, `ui-artifacts/scan.py` | Дополнительные oracle/сборщики: harmless browser execution marker, OOB JSONL callbacks, benign upload/storage hash checks, static asset metadata. Не гарантируется, что sweep их автоматически связал. |
| `H/supervisor/` | Построение obligations из triage/findings/verification/outcomes/journal и completion gate. |
| `H/playbooks/`, `TRIAGE.md`, `CLASS-REGISTRY.json` | Процедуры и taxonomy; наличие инструкции не равно доступности адаптера/прогона. Registry содержит устаревшие capability flags (8). |
| `H/ledger.py`, `coverage_go.py`, `sweep_coverage.py`, `sweep_report.py` | Fingerprint/история результатов, bridge в Go coverage, локальный builder и Markdown report. |

## 3. Потоки данных и состояние

### 3.1. Сбор трафика: recon → Caido → caido-tail → projection → graph

```text
Оператор/разрешённый recon/browser
  -> HTTP(S) proxy -> Caido: выбранный проект/history
  -> pinned Caido MCP read-source
  -> caido-tail / caido-import / caido-sync
  -> graph.sqlite3: metadata + revision registry + projection_jobs + checkpoint
  -> projection worker (project maintenance lease)
  -> behavior.sqlite3 + receipts + neo4j_outbox
  -> Neo4j project projection / generation
  -> appquery -> UI / MCP / CLI
```

1. **Получить трафик.** Вне продукта recon/browser должен использовать предназначенный proxy и проверить активный проект Caido. Метаданные recon лежат в `H/routes.json`, `input-points.json` либо run/recon. У Python recon сегодня есть прямой путь к Caido, минующий gateway: это текущая реализация, а не исправленная схема (8.1).
2. **Выбрать namespace.** Project key — SHA-256 канонического JSON с `instance_id` и `project_id`; display name в identity/path не участвует. Capture observation — отдельный SHA-256 с `request_id` (`G/internal/project/identity.go:34–69`). Реестр — `<data-root>/projects/registry.json`.
3. **Открыть источник.** `internal/caido` запускает pinned MCP executable и ограниченный read API. Источник сверяется по instance/project, доступ к capture details — после scope filtering. Для live scope нормализованная тройка scheme/host/port входит и в checkpoint source key (`G/internal/ingest/live_tail.go:134–155`).
4. **Historical intake.** `PageWriter` пишет AES-GCM vault objects и immutable redacted receipts; затем в транзакции фиксирует captures, revisions, search projection, jobs, digests и cursor (`G/internal/ingest/page_writer.go:188–190`, `G/internal/graph/projection_jobs.go:66–69`). Состояния history: running/partial/done. Отдельные file writes и SQLite transaction — разные failure domains.
5. **Live intake.** `live_sync_state` держит watermark, overlap-window/cursor и независимый response-refresh cursor; `live_capture_index` — структурные поля/подписи, revision mapping — в v7 tables. Defaults: page 50, body limit 256 KiB, overlap 90 s, max pages 1 (`G/internal/ingest/live_tail.go:24–79`, `G/internal/store/schema.go:100–145,190–205`). Raw live bytes остаются в Caido; будущий UI read не должен выдавать live projection за исторический encrypted объект.
6. **Durable handoff.** Job ID `projection-<revision_id>`, checksum нормализованного bundle; повтор той же revision/payload идемпотентен, другая payload для той же revision — conflict (`G/internal/graph/projection_jobs.go:66–131`). Checkpoint может быть продвинут при ещё pending projection: принятие source и доступность graph различаются.
7. **Projection.** Независимый worker claim/lease/retry/dead-letter, локальный graph import и Neo4j sync, затем ack/mapping. Defaults: lease 10 m, 8 attempts, retry 5 s → cap 15 m (`G/internal/ingest/projection_worker.go:20–27,122–140`). Всё под exclusive project maintenance lease; writer повторно используется в batch, но validated (`:165–198`). `serve --project-key …` включает таких workers; `caido-sync` также имеет собственные runners. Не запускать конкурирующие владельцы без понимания lock contention.
8. **Чтение/полнота.** Neo4j snapshot parity и source history completeness — разные проверки. `neo4j-backfill --verify-only`, `caido-reconcile`, `projection-reconcile` отвечают на разные вопросы; successful graph read не означает всю историю. Состояние backlog/error остаётся в SQLite, не только в логах.

### 3.2. Охота: gate → gateway → recon → triage → специалисты → verify → supervisor → отчёт/ledger

Порядок ниже взят из **shell-кода**, а не из описания фаз в SWEEP.md.

1. **Plan/execute boundary.** Без `--execute` только plan. С execute создаётся новый run dir, в `run/recon` копируется уже существующий локальный inventory; в `run/.bb-assistant-build.*` собирается Go CLI из `BB_ASSISTANT_SOURCE`, а не берётся случайный PATH binary (`H/sweep.sh:191–239`). Промежуточный build dir удаляется trap.
2. **Gate до gateway.** `bb-assistant gate` проверяет target, Caido, recon, optional LLM; JSON/log — `gate.json`, `gate.log`. Caido URL жёстко указан `http://10.10.10.10:8080`, даже если gateway upstream изменён. Invalid/несогласованный exit/JSON останавливают sweep до triage (`H/sweep.sh:241–311`). Gate делает свои target network probes до запуска policy proxy; следовательно gateway не покрывает весь run.
3. **Gateway.** Генерируется `policy.json`: exact HTTP origin/path-prefix, read methods; с opt-in добавляются POST/PUT/PATCH, но DELETE не добавляется. HTTPS получает CONNECT allowlist без visibility внутренних методов/path/RPS. Gateway слушает loopback, пишет `toolcalls.jsonl`; только после readiness выставляются proxy ENV и очищается NO_PROXY (`H/sweep.sh:313–379`). Это требование к добросовестным дочерним клиентам, не принудительная маршрутизация.
4. **Recon optional.** `governor.py run … HUNTER_RECON_RUNNER` пишет `recon-generated`, затем routes/input-points копируются в `recon/` (`:387–394`). В sweep **нет второго gate после этого обновления**: первый gate видел staged inventory. Реальная свежесть и пригодность нового recon отдельным readiness receipt не подтверждаются.
5. **Triage.** Runner получает только target/classes/TRIAGE/routes/input-points, optional surface; результат `triage.json`. API surface берётся optional из `BB_ASSISTANT_API`+project key либо файла, недоступность нефатальна (`H/runners/README.md:38–40`, `runners/graph_surface.py`). Graph hints — кандидаты, не finding. `sweep.sh` фильтрует результат своим списком **11** классов (`:408–414`), хотя standalone triage знает 38.
6. **Специалисты.** Class contexts — `specialists/<class>.context.json`; Hermes запуск волнами максимум 3. Runner при отсутствии `specialists/identities.json` сначала вызывает bootstrap двух акторов, **не проверяя `--allow-mutations`** (`H/runners/hunter-agent-runner:1065–1085`). JSON-контракт finding требует observed effect, existing refs, replayable PoC, structured expected_effect; одна correction pass. На ошибках runner заранее оставляет `{"findings":[]}` (`:1052–1055`): пустой output не должен трактоваться как отрицательный результат.
7. **Merge/fingerprint и coverage ДО verify.** Findings/outcomes объединяются в `findings.json`, `coverage-outcomes.json`, добавляется fingerprint. `coverage_go.py` формирует временную SQLite Go-схемы и сравнивает JSON Go CLI с локальной матрицей; Go result копируется в `coverage.json` (`H/sweep.sh:575–584`). Эта матрица построена по заявлениям специалистов, **ещё не по итогам class verification**. После проверки shell её не пересчитывает; supervisor должен отдельно учитывать отрицательные/blocked/inconclusive результаты.
8. **Verify.** Sweep всегда передаёт `--class-verifiers`, optional mutation/callback flags. Результат — `verification.json` (`:586–597`). Класс получает plugin result `PASS/FAIL/INCONCLUSIVE/BLOCKED`, generic replay — другой vocabulary. CLI не передаёт browser adapter: XSS browser plugins блокируются, даже если `browser-runner/` установлен (`H/verify/verify.py:1068–1072`).
9. **Ledger ДО supervisor.** `ledger.py record` записывает результаты сразу после verify (`H/sweep.sh:598–600`). Затем `build_obligations.py` собирает hypotheses/checks/budget/deadline из triage/findings/verification/outcomes и числа строк gateway journal; `supervisor.py check` → `supervisor.json` (`:603–608`). Таким образом запись ledger не означает, что supervisor разрешил DONE.
10. **Закрытие и отчёт.** Shell сверяет exit/status/reasons, дополнительно не допускает DONE при untested/blocked/open coverage, генерирует `sweep-report.md`, возвращает 0/2/3 и останавливает gateway (`H/sweep.sh:613–694,216–231`). Supervisor не создает coverage доказательства, а проверяет уже заявленные obligations. Report tool-call metric и supervisor count имеют разные источники (8).

## 4. Хранилища, идентификаторы и версии схем

### 4.1. Файловая раскладка продукта

`<data-root>`: `BB_ASSISTANT_DATA_HOME` → `XDG_DATA_HOME/bb-graph-assistant` → `~/.local/share/bb-graph-assistant`; без explicit override код может предпочесть существующий вложенный service registry `.../graph-kb-assistant` (`G/internal/project/paths.go:20–48`). Поэтому пути local Kali и CT108 нельзя молча объединять.

```text
<data-root>/
  projects/
    registry.json                       # registry v1
    inventory-state.json                # inventory missing confirmations v1
    <project_key>/
      .maintenance.lock
      graph.sqlite3                     # capture store, PRAGMA user_version=7
      behavior.sqlite3                  # BehaviorGraph metadata.schema_version=2
      consultation.sqlite3              # PRAGMA user_version=4
      behavior-receipts/
      vault/<observation>.<side>.<sha256>.bba
  coverage.sqlite3                      # global/run-keyed coverage, no version marker
  query-state/full-cursors.sqlite3        # persistent CLI full-view cursors, schema v1
```

Основания layout: `G/internal/store/project.go:107–167,191–222`, registry/inventory `internal/project/registry.go:16–42`, `inventory.go:15,59`; консультации `internal/consultation/sqlite.go:22–24`. Persistent full-view cursor state для CLI хранится в `<data-root>/query-state/full-cursors.sqlite3`, schema v1 (`G/internal/appquery/graph.go:238–251`, `full_cursor.go:102–155`; CLI factory `G/cmd/bb-assistant/main.go:1786–1788`). Штатный UI/MCP `NewTools` вызывает обычный `NewGraphQueryService`, где full-cursors в памяти и surface signing key случайный на процесс (`G/internal/mcpserver/server.go:161–162`, `G/internal/appquery/graph.go:231–235`). Поэтому continuation после restart у CLI и long-running UI/MCP имеет разный lifecycle; нельзя обещать persistent UI cursor только по наличию SQLite implementation.

### 4.2. Capture store и vault

| Слой | Схема/данные | Контракт |
|---|---|---|
| `graph.sqlite3` | v7 (`G/internal/store/store.go:20`); `project_meta`, `captures`, `capture_revisions`, `graph_capture_map`, `graph_capture_revisions`, `capture_search`, `capture_search_fts` | Capture identity отделена от revision. Исторические raw refs ведут в encrypted vault. |
| History checkpoints | `sync_state`, `sync_runs`, `sync_run_cursor_digests`, `sync_run_request_digests` | Resume страницы/счётчики, обнаружение cursor/request replay. |
| Live metadata | `live_sync_state`, `live_capture_index`, `live_capture_fts`, `live_graph_capture_revisions` | Watermark/window/cursor/response-refresh, signatures, graph IDs, parse complete/partial; не vault messages. |
| Revision registry/queue | `capture_revision_registry`, `projection_jobs` | v6 добавила durable handoff, v7 — live refresh/revision mapping. |
| `.bba` vault | Magic `BBA1`, nonce, AES-256-GCM ciphertext/tag | Ключ Base64URL → ровно 32 bytes из `BB_ASSISTANT_VAULT_KEY`; filename включает plaintext SHA-256; AAD связывает observation/side. Mode 0600, immutable hard-link publication + fsync (`G/internal/ingest/vault.go:22,64–153`). Сам ключ не в backup. |

Полная DDL — `G/internal/store/schema.go:4–206`. Capture revisions содержат request/response hashes/sizes/refs, truncation и wire fidelity `source_bytes/reconstructed_text/unknown` (`:62–79`). Даже authenticated vault object не гарантирует, что Caido MCP передал оригинальные wire bytes, а не реконструированное сообщение.

Go открывает существующую DB только v7; не мигрирует её автоматически, Python владеет upgrades (`G/internal/store/project.go:27–51`, `schema.go:157–193`, `store.go:155–156`). Новая Go DB создаётся WAL/synchronous FULL (`project.go:228–258`). Private root проверяется против source checkout и network filesystem; директории 0700 (`project/paths.go:68–105`).

### 4.3. BehaviorGraph, outbox и очередь

- `behavior.sqlite3`: `metadata`, `sources`, `entities`, `assertions`, `cards`, `imports`, `entity_source_refs`; payload records в JSON, metadata schema **2**. Import bundle wire schema **1**, snapshot schema **2** — это разные числа (`G/internal/graph/writer.go:411–414,1213–1227`; `reader.go:133`). Analysis-card schema 1, import/receipt schema 1.
- Наблюдаемые сущности: `asset`, `endpoint`, `operation`, `observation`, `parameter`, `field_occurrence`, `value_reference`, `request_variant`, `response_variant`; assertions соединяют typed IDs и несут context/source refs (`G/internal/graph/historical.go:169,251–265,325–418,536`). Value correlation — наблюдаемое соответствие подписей/значений, не доказательство потока управления или права доступа.
- Neo4j использует `GraphRecord` с record type/kind/project key, `GraphImport` bundle identity, `GraphProject` generation; constraints уникальности record/import/project и query indexes (`G/internal/neo4jstore/store.go:186–195`). Публикация snapshot/rebuild не смешивает проекты, но нуждается в корректной generation/cursor policy.
- `projection_jobs` находится **в capture DB**, а `neo4j_outbox` — **в BehaviorGraph DB** (`G/internal/store/schema.go:167–187`; `G/internal/graph/writer.go:909`). Это две очереди разных уровней.
- Job: sequence ID, unique job/revision ID, payload JSON/hash, `pending/leased/applied/dead_letter`, attempt count, next attempt, lease token/until, last error/applied time. Check требует lease fields только для leased. Job immutable/idempotent по revision и normalized payload; checksum и source/project identity проверяются при чтении/доставке (`G/internal/graph/projection_jobs.go:36–69,108–131`).

### 4.4. Consultation и KB

- `consultation.sqlite3` v4: sessions, comparisons, proposals, receipts, candidates, reviews, context annotations, operation aliases, idempotency (`G/internal/consultation/sqlite.go:124–217`). Mutations имеют expected version/idempotency key; saved candidate остаётся `candidate`, receipt подтверждает exact references, не semantic impact (`G/internal/mcpserver/consultation.go:313–332`).
- Evidence packet wire version `1`, redaction policy `bb-evidence-redaction-v1`: максимум 4 selections, 4096 bytes/span, 8192 selected bytes суммарно; redaction heuristic требует operator review (`G/internal/appquery/evidence_packet.go:15–35,98–114`).
- KB default index `<data-root>/kb/index.sqlite3`; override `BB_ASSISTANT_KB_INDEX`/CLI index. Отдельный ENV может указывать sibling runtime KB, что и делают deployment templates.
- KB SQLite: `kb_meta`, `source_documents`, `chunks`, `chunk_embeddings`, FTS5 `chunk_search` (`G/src/bb_assistant/kb/index.py:157–205`). В этой DDL нет общего `PRAGMA user_version`; не выдумывать «KB DB v7». Canonical chunk contract — **`redamon-compatible-chunk-v2`**, source manifest schema **1**, parity receipts schema **3** (`canonical.py:9`; `sources.yaml:5`; `parity.py:220`).
- Snapshot identity/coverage hashes/source revisions/byte spans отделены от vector-ready marker. E5 `intfloat/e5-large-v2`, 1024 dimensions; локальная выдача может быть lexical-only. Опциональные BGE reranking/RRF/MMR и `redamon` worker backend не доказывают corpus/result parity (`G/internal/kb/index.go:68–72,387,948,1104–1137`; `G/internal/mcpserver/server.go:180–218`).

### 4.5. Состояние охоты

- В run dir: `gate.json/log`, `policy.json`, `toolcalls.jsonl`, `.governor-state.json` + `.lock`, `governor-degraded`, `recon/`, `recon-generated/`, `triage.json`, `class-list.txt`, `specialists/*`, `findings.json`, `verification.json`, `coverage-outcomes/local/go.json`, `coverage.json`, `obligations.json`, `supervisor.json/log`, `ledger-summary.json`, `sweep-report.md`, `sweep.log`. Это не единый transactional run store (`H/sweep.sh:191–694`).
- `specialists/identities.json` содержит synthetic **секреты**, не публичный evidence artifact; bootstrap сначала пишет файл, затем chmod 0600 (`H/runners/identities.py:65–71`). Не включать его в пакет внешнего ревью.
- Ledger по умолчанию `H/findings-ledger.json`, override `HUNTER_FINDINGS_LEDGER`; JSON `{version:1, entries:[…]}` (`H/ledger.py:16,42–43`). Entries: finding_id/class/endpoint/method/fingerprint/payload_sha256/status/verified_by/date/evidence/history (`:144–180`). Exact-request fingerprint не включает class. На совпадении обновляются текущие поля и history. Несмотря на docstring «append-only», физический файл перезаписывается; межпроцессного lock/atomic replacement в `_write_json` нет (`:272–280`).
- Coverage: `<data-root>/coverage.sqlite3`, `coverage_cells(run,class,status,tests,budget,evidence_json)`, PK `(run,class)`, допустимые статусы `confirmed/ruled_out/untested/blocked_first_party`; **явной DB schema version нет** (`G/internal/coverage/sqlite.go:16–17,79–87`). В sweep этот data-root — `run/.bb-assistant-build.*/coverage-data`, удаляемый при завершении; durable run artifact — JSON, не автоматически global product coverage DB (`H/sweep.sh:203,226–228,577–583`).
- Gateway `policy.json` и `.governor-state.json` не имеют явного version field в текущих builders/loader; их shape задаётся Python validation/state code (`H/sweep.sh:333–344`, `H/gateway/gateway.py:631–633`, `H/governor.py:82–101`). Run findings/verification/report также не объединены versioned run manifest.
- Obligations JSON не имеет schema-version field: hypotheses/checks/budget; statuses `open/tested/blocked/disposition`, checks `pass/fail/inconclusive/blocked`, budget `{max_calls,calls_used,deadline_utc}` (`H/supervisor/obligations.py:10–52,133–162`).

## 5. Интерфейсы

### 5.1. HTTP API `127.0.0.1:8765`

Default адрес подтверждён `G/internal/managedapp/app.go:40–41`, `G/cmd/bb-assistant/main.go:2019–2025`. Ниже **21 явная пара метод+маршрут `/api/*`** из `G/internal/webui/server.go:104–127`. Это каталог исходников, а не результат live route discovery.

Обозначения: `project_key` — 64 lowercase hex; **S** = `project_key, session_id, expected_version, idempotency_key` в JSON body; POST требует `application/json` и совпадающий `Origin: http://127.0.0.1:8765`. Указаны все поля top-level request; обязательность/дополнительные ограничения задаются соответствующим DTO/use case, а не URL router. Consultation routes регистрируются при реализации `consultationAPI`, maintenance — `operationBackfillAPI`; production `Tools` используется handler-ом (`server.go:112–127`, `managedapp/app.go:96–114`).

| Метод | Маршрут | Параметры и смысл | Регистрация; контракт |
|---|---|---|---|
| GET | `/api/health` | Нет. Ответ status/version; не проверяет backend connectivity. | `G/internal/webui/server.go:104`; `G/internal/webui/server.go:150–155` |
| GET | `/api/projects` | Query: after_project_key?, limit? (1–200, default 200). Локальный catalog/coverage. | `G/internal/webui/server.go:105`; `G/internal/webui/server.go:176–197` |
| GET | `/api/runtime-capabilities` | Нет. evidence_vault_key_state: missing_or_invalid/configured_not_verified/verified. | `G/internal/webui/server.go:106`; `G/internal/webui/server.go:144–147` |
| GET | `/api/surface` | Query: project_key, filter? (≤256 bytes), cursor?, limit? (1–100, default 40). | `G/internal/webui/server.go:107`; `G/internal/webui/server.go:200–255` |
| GET | `/api/graph` | Query: project_key, seed (повтор 1–20 unique), view?=direct\|full, cursor?. У этого HTTP route нет max_depth/max_nodes/max_edges query API. | `G/internal/webui/server.go:108`; `G/internal/webui/server.go:259–306` |
| GET | `/api/evidence` | Query: project_key, request_id, side=request\|response; revision_id?, offset?≥0, max_bytes?. Чувствительные historical bytes. | `G/internal/webui/server.go:109`; `G/internal/webui/server.go:310–367` |
| GET | `/api/compare` | Query: project_key, left_request_id, left_revision_id, right_request_id, right_revision_id. Обе exact revision IDs обязательны. | `G/internal/webui/server.go:110`; `G/internal/webui/server.go:371–408` |
| POST | `/api/evidence/packet-preview` | Body: project_key, question, selections:[{request_id,revision_id,side,offset,length}]. Redacted preview без model call. | `G/internal/webui/server.go:111`; `G/internal/webui/server.go:412–435; G/internal/appquery/evidence_packet.go:23–35` |
| GET | `/api/consultation/session` | Query: project_key, session_id. Resume saved session. | `G/internal/webui/server.go:113`; `G/internal/webui/consultation.go:152–168` |
| GET | `/api/consultation/review` | Query: project_key, session_id. Local operator review view. | `G/internal/webui/server.go:114`; `G/internal/webui/consultation.go:171–186` |
| POST | `/api/consultation/session` | Body: project_key, question, selections, idempotency_key. Create session/evidence packet. | `G/internal/webui/server.go:115`; `G/internal/webui/consultation.go:23–28,190–201` |
| POST | `/api/consultation/accept-review` | Body: S. Записать operator acceptance текущей версии. | `G/internal/webui/server.go:116`; `G/internal/webui/consultation.go:68–73,205–215` |
| POST | `/api/consultation/context-annotation` | Body: S + kind, label, evidence_reference_ids. | `G/internal/webui/server.go:117`; `G/internal/webui/consultation.go:75–83,218–232` |
| GET | `/api/consultation/operation-aliases` | Query: только project_key. Список project aliases. | `G/internal/webui/server.go:118`; `G/internal/webui/consultation.go:93–104` |
| POST | `/api/consultation/operation-alias` | Body: project_key, operation_id, expected_version, idempotency_key, alias. Проверяет operation в выбранном graph. | `G/internal/webui/server.go:119`; `G/internal/webui/consultation.go:85–91,107–134` |
| POST | `/api/consultation/compare` | Body: S + left_request_id, left_revision_id, right_request_id, right_revision_id. | `G/internal/webui/server.go:120`; `G/internal/webui/consultation.go:30–39,235–248` |
| POST | `/api/consultation/proposal` | Body: S + proposal, evidence_reference_ids, comparison_ids. Сохранить недоверенный proposal, не confirmed finding. | `G/internal/webui/server.go:121`; `G/internal/webui/consultation.go:41–49,251–265` |
| POST | `/api/consultation/validate` | Body: S + proposal_id. Receipt exact citation integrity, не semantic verification. | `G/internal/webui/server.go:122`; `G/internal/webui/consultation.go:51–57,268–281` |
| POST | `/api/consultation/candidate` | Body: S + proposal_id, receipt_id. Сохранить candidate version. | `G/internal/webui/server.go:123`; `G/internal/webui/consultation.go:59–66,284–297` |
| POST | `/api/maintenance/operation-backfill/preview` | Body: project_key, max_captures?, max_request_bytes?, cursor?. candidate_sha256 должен отсутствовать/быть пустым. Возвращает bounded preview. | `G/internal/webui/server.go:126`; `G/internal/webui/server.go:439–480,484–505` |
| POST | `/api/maintenance/operation-backfill/apply` | Body: project_key, candidate_sha256 (64 hex), max_captures?, max_request_bytes?; cursor непустой запрещён. Изменяет projection после preview digest. | `G/internal/webui/server.go:127`; `G/internal/webui/server.go:439–480,506–535` |

Nested `proposal` — объект `title, summary, rationale, impact, alternatives[], next_checks[], limitations[]` (`G/internal/consultation/service.go:178–186`). Evidence selection fields заданы DTO `G/internal/appquery/evidence_packet.go:23–35`; это exact historical revision/span, не live lookup. JSON body decoders запрещают unknown fields; cap consultation 96 KiB, packet preview 16 KiB, operation backfill 4 KiB (`G/internal/webui/consultation.go:18,300–315`; `server.go:419,453`).

**Статика, не включённая в число API:** `GET /` (`G/internal/webui/server.go:93`), `GET /assets/app.js` (`:100`), `GET /assets/graph-focus.js` (`:101`), `GET /assets/runtime-capabilities.js` (`:102`), `GET /assets/style.css` (`:103`). Всего в web UI router 26 явных регистраций. Go GET patterns также обслуживают HEAD; они не посчитаны отдельными явно объявленными маршрутами. Неизвестный путь под `/` отклоняется проверкой path, это не универсальный API wildcard (`:93–99`).

**Дополнительно `/mcp`:** только managed `serve` монтирует `mux.Handle("/mcp", …)` (`G/internal/managedapp/app.go:106–114`); `web-ui` отдельно этот endpoint не монтирует. Это один дополнительный endpoint с MCP SDK protocol handling, **не** ещё один JSON REST API. В project router методы `/mcp` не перечислены; нельзя приписывать ему собственные invented GET/POST routes по аналогии. SDK получает JSONResponse, session timeout 30 m, max request body 1 MiB, cross-origin protection; local-host protection отключена под authenticated reverse proxy (`G/internal/mcpserver/streamable_http.go:13,64–75`).

UI security gate: только loopback Host, при наличии Origin он должен совпасть; POST из allowlist требует Origin. Общая authentication внутри Go UI отсутствует — внешняя Basic Auth у Nginx. `GET /api/evidence` отдаёт **чувствительные** bytes/encoding, а не redacted packet; это не тот же барьер, что opt-in raw MCP (`G/internal/webui/server.go:643–669,748–798`).

### 5.2. MCP `bb_*`

В source catalog **16 штатных инструментов + 1 opt-in**. Список ниже учитывает append из coverage и consultation, а не только literal `catalog := …`. Stdio entrypoint `bb-assistant mcp-server`; managed HTTP — `/mcp`. MCP не делает target requests; consultation tools при этом **пишут локальное состояние**, несмотря на общее описание сервера «Read-only local assistant».

| Инструмент | Назначение/параметры | Источник |
|---|---|---|
| `bb_consult_create_session` | Создать/idempotently resume question+selected exact historical spans; content withheld до operator acceptance. | `G/internal/mcpserver/consultation.go:299` |
| `bb_consult_compare` | Сохранить deterministic comparison exact captures в accepted consultation. | `G/internal/mcpserver/consultation.go:306` |
| `bb_consult_record_proposal` | Сохранить untrusted proposal и evidence/comparison references. | `G/internal/mcpserver/consultation.go:313` |
| `bb_consult_validate_proposal` | Перечитать exact references/re-run compare; receipt integrity, не proof of impact. | `G/internal/mcpserver/consultation.go:320` |
| `bb_candidate_save` | Создать immutable candidate version при matching receipt; не verified/reproduced state. | `G/internal/mcpserver/consultation.go:325` |
| `bb_consult_resume` | Прочитать saved consultation/proposals/receipts/candidates после restart. | `G/internal/mcpserver/consultation.go:331` |
| `bb_coverage_matrix` | run → saved local matrix, can_finish/open/coverage_pct; ошибок missing run не скрывает. | `G/internal/mcpserver/coverage.go:42` |
| `bb_list_projects` | Локальные project identities/coverage; after_project_key, limit. | `G/internal/mcpserver/server.go:1189` |
| `bb_search_captures` | Structural search: project_key или project_keys (не оба), text/method/host/route/status/field/signatures/time/limit; не raw values. | `G/internal/mcpserver/server.go:1197` |
| `bb_surface_map` | Operations одного project_key, filter/cursor/limit; bounded field names/seeds. | `G/internal/mcpserver/server.go:1202` |
| `bb_graph_slice` | project_key, seed_ids, cursor?, max_nodes/max_edges/max_depth 0–6; bounded graph/provenance. | `G/internal/mcpserver/server.go:1212` |
| `bb_graph_neighborhood` | project_key, seed_ids (до 20), cursor?; ровно one-hop, max_depth только 1. | `G/internal/mcpserver/server.go:1224` |
| `bb_kb_stats` | KB snapshot/source/vector/manifest freshness statistics; без fetch. | `G/internal/mcpserver/server.go:1243` |
| `bb_kb_search` | query, top_k, source_names, min_cvss, severity, include_supplemental; citations и untrusted excerpts. | `G/internal/mcpserver/server.go:1248` |
| `bb_kb_get_source` | source_name, source_id, max_chars; exact source document до 30000 chars. | `G/internal/mcpserver/server.go:1260` |
| `bb_get_capture` | project_key + Caido request_id; redacted structural summary, не graph observation ID и не raw. | `G/internal/mcpserver/server.go:1270` |
| `bb_get_capture_raw` | Opt-in sensitive historical vault range: project_key/request_id/side/offset/max_bytes ≤16384. Нет live fetch. | `G/internal/mcpserver/server.go:1281` |

В реально прочитанном `P/bb-hunter/config.yaml:13–28` 15 имён: перечисленные выше без coverage и raw. Значит наличие `bb_coverage_matrix` в checkout не гарантирует его доступность охотнику. Main profile подключает удалённый MCP через `/home/max/.local/bin/bb-assistant-mcp-pve` → restricted SSH user `bb-assistant-mcp@proxmox`; это не автоматически localhost server.

### 5.3. CLI `bb-assistant`

**Полный список 33 команд** взят из единой usage-строки `G/cmd/bb-assistant/main.go:114`, наличие каждого подтверждено switch `:117–183`. Ниже краткое назначение и usage/реальная flag declaration. Это верхнеуровневые команды Go binary, не все Python KB maintenance subcommands и не scripts wrappers.

| Команда | Назначение | Синтаксис/основные параметры из кода | Подтверждение |
|---|---|---|---|
| `gate` | Readiness probes target/Caido/recon/optional LLM; nonzero останавливает caller. | `gate --target URL --caido-url URL --recon-dir DIR [--json]; thresholds and timeout must be positive` | `G/cmd/bb-assistant/main.go:117`; `G/cmd/bb-assistant/gate.go:50` |
| `version` | Вывести build/version string. | `version` | `G/cmd/bb-assistant/main.go:121`; `G/cmd/bb-assistant/main.go:122` |
| `coverage` | Прочитать сохранённую матрицу run, вычислить can_finish/open/coverage_pct. | `coverage --run ID [--json] [--data-root path]` | `G/cmd/bb-assistant/main.go:124`; `G/cmd/bb-assistant/coverage.go:30` |
| `status` | Локальная runtime/config/store диагностика без обещания live source completeness. | `status [--data-root path]` | `G/cmd/bb-assistant/main.go:126`; `G/cmd/bb-assistant/status.go:22` |
| `db-check` | Проверить совместимость существующей capture SQLite DB; не migration. | `db-check <existing-project.sqlite3>` | `G/cmd/bb-assistant/main.go:128`; `G/cmd/bb-assistant/main.go:1897` |
| `project-audit` | Проверить выбранный project store, graph и capability summary. | `project-audit --project-key <sha256> [--data-root path]` | `G/cmd/bb-assistant/main.go:130`; `G/cmd/bb-assistant/main.go:416` |
| `project-backup` | Ограниченный архив project files или dry-run, без vault key. | `project-backup --project-key <sha256> --max-bytes <positive> --max-files <positive> [--output <new-archive>] [--dry-run] [--data-root path]; --output is required unless --dry-run is set` | `G/cmd/bb-assistant/main.go:132`; `G/cmd/bb-assistant/main.go:441` |
| `project-restore` | Восстановить bounded archive в private root и вывести audit summary. | `project-restore --input <archive> --max-bytes <positive> --max-files <positive> [--data-root path]` | `G/cmd/bb-assistant/main.go:134`; `G/cmd/bb-assistant/main.go:471` |
| `neo4j-check` | Read-only schema check, либо создать недостающую schema с ensure-schema. | `neo4j-check [--ensure-schema]` | `G/cmd/bb-assistant/main.go:136`; `G/cmd/bb-assistant/main.go:266` |
| `neo4j-backfill` | Rebuild Neo4j из locked local snapshot/read-back parity; verify-only не заменяет snapshot. | `neo4j-backfill --project-key <sha256> [--data-root path]` | `G/cmd/bb-assistant/main.go:138`; `G/cmd/bb-assistant/main.go:303` |
| `neo4j-sync` | Доставить существующий Neo4j outbox выбранного проекта. | `neo4j-sync --project-key <sha256> [--data-root path]` | `G/cmd/bb-assistant/main.go:140`; `G/cmd/bb-assistant/main.go:199` |
| `projection-worker` | Drain durable projection_jobs; optional watch. | `projection-worker --project-key <sha256> [--max-jobs 10] [--lease-duration 10m] [--max-attempts 8] [--watch --poll-interval 1s] [--data-root path]` | `G/cmd/bb-assistant/main.go:142`; `G/cmd/bb-assistant/main.go:559` |
| `projection-reconcile` | Сопоставить queue/local/Neo4j; apply ack только exact matches после parity. | `projection-reconcile --project-key <sha256> [--include-dead-letter] [--apply] [--max-jobs 5000] [--max-bytes 536870912] [--after-sequence-id 0] [--data-root path]` | `G/cmd/bb-assistant/main.go:144`; `G/cmd/bb-assistant/main.go:661` |
| `projection-replay` | Preview/apply повторного включения явно выбранных dead-letter revisions. | `projection-replay --project-key <sha256> --revision-id <sha256> [--revision-id <sha256> ...] [--apply] [--data-root path]` | `G/cmd/bb-assistant/main.go:146`; `G/cmd/bb-assistant/main.go:792` |
| `graph-operation-backfill-preview` | Bounded preview недостающей operation projection из historical captures. | `graph-operation-backfill-preview --project-key <sha256> [--max-captures 5000] [--max-request-bytes 536870912] [--data-root path]` | `G/cmd/bb-assistant/main.go:148`; `G/cmd/bb-assistant/main.go:940` |
| `graph-operation-backfill-apply` | Применить только preview с совпавшим candidate digest. | `graph-operation-backfill-apply --project-key <sha256> --candidate-sha256 <preview-sha256> [--max-captures 5000] [--max-request-bytes 536870912] [--data-root path]` | `G/cmd/bb-assistant/main.go:150`; `G/cmd/bb-assistant/main.go:1012` |
| `graph-navigation-backfill-preview` | Preview navigation/operation hints из bounded historical data. | `graph-navigation-backfill-preview --project-key <sha256> [--max-captures 5000] [--max-bytes 536870912] [--data-root path]` | `G/cmd/bb-assistant/main.go:152`; `G/cmd/bb-assistant/main.go:1094` |
| `graph-navigation-backfill-apply` | Применить navigation backfill по проверенному preview digest. | `graph-navigation-backfill-apply --project-key <sha256> --candidate-sha256 <preview-sha256> [--max-captures 5000] [--max-bytes 536870912] [--data-root path]` | `G/cmd/bb-assistant/main.go:154`; `G/cmd/bb-assistant/main.go:1166` |
| `caido-reconcile` | Сверить bounded Caido history inventory с локальными records; не graph snapshot parity. | `caido-reconcile --project-key <sha256> [--scope-host <host> --scope-port <port>] [--page-size 50] [--max-pages 100] [--max-requests 10000] [--timeout 30s]` | `G/cmd/bb-assistant/main.go:156`; `G/cmd/bb-assistant/main.go:1365` |
| `kb-validate` | Проверить snapshot directory/schema/hash, не получить новые источники. | `kb-validate <snapshot-directory>` | `G/cmd/bb-assistant/main.go:158`; `G/cmd/bb-assistant/main.go:1929` |
| `kb-search` | Локальный bounded KB retrieval по query/source/top-k. | `kb-search --query <text> [--top-k 10] [--source <family>] [--index path]` | `G/cmd/bb-assistant/main.go:160`; `G/cmd/bb-assistant/main.go:1959` |
| `mcp-server` | Stdio MCP сервер над existing registry/optional KB. | `mcp-server [--data-root path] [--kb-index path]` | `G/cmd/bb-assistant/main.go:162`; `G/cmd/bb-assistant/main.go:1987` |
| `mcp-tools` | JSON tool catalog; raw tool зависит от ENV. | `mcp-tools` | `G/cmd/bb-assistant/main.go:164`; `G/cmd/bb-assistant/main.go:2003` |
| `web-ui` | Standalone loopback UI/API, без managed projection workers и /mcp. | `web-ui [--data-root path] [--kb-index path] [--addr 127.0.0.1:8765]` | `G/cmd/bb-assistant/main.go:166`; `G/cmd/bb-assistant/main.go:2025` |
| `serve` | Managed UI + /mcp + явно выбранные project workers. | `serve [--project-key sha256 ...] [--data-root path] [--kb-index path] [--addr 127.0.0.1:8765] [--poll-interval 1s] [--max-jobs 10] [--lease-duration 10m] [--max-attempts 8]` | `G/cmd/bb-assistant/main.go:119`; `G/cmd/bb-assistant/serve.go:31` |
| `projects` | Перечислить локальные registered Caido projects. | `projects [--data-root path]` | `G/cmd/bb-assistant/main.go:168`; `G/cmd/bb-assistant/main.go:2070` |
| `caido-register` | Создать локальный namespace по opaque instance/project IDs, не переключая Caido. | `caido-register --instance-id <stable-key> --project-id <id> [--name <display-name>]` | `G/cmd/bb-assistant/main.go:170`; `G/cmd/bb-assistant/main.go:2106` |
| `search` | Структурный capture search по явно выбранным project keys/filters. | `search --project-key <sha256> [--project-key <sha256>] [filters]` | `G/cmd/bb-assistant/main.go:172`; `G/cmd/bb-assistant/main.go:2181` |
| `graph-slice` | Bounded graph traversal по seed IDs/cursor/limits. | `graph-slice --project-key <sha256> --seed <typed-id> [--seed <typed-id>] [--cursor token] [limits]` | `G/cmd/bb-assistant/main.go:174`; `G/cmd/bb-assistant/main.go:1809` |
| `graph-import` | Импорт validated graph bundle выбранного проекта (запись). | `graph-import --project-key <sha256> <bundle.json>` | `G/cmd/bb-assistant/main.go:176`; `G/cmd/bb-assistant/main.go:1699` |
| `caido-import` | Bounded resumable historical import из Caido (local writes). | `caido-import --project-key <sha256> [--page-size 50] [--max-pages 10]` | `G/cmd/bb-assistant/main.go:178`; `G/cmd/bb-assistant/main.go:1588` |
| `caido-tail` | Live metadata/projection intake, optional scoped watch/response refresh. | `caido-tail --project-key <sha256> [--scope-scheme <http\|https> --scope-host <host> --scope-port <port>] [--require-scope] [--page-size 50] [--max-pages 1] [--watch --poll-interval 10s]` | `G/cmd/bb-assistant/main.go:180`; `G/cmd/bb-assistant/main.go:1446` |
| `caido-sync` | Continuous scoped project inventory + bounded history/live intake/projection; absent projects retained. | `caido-sync --instance-id ID --scope-origin ORIGIN [--scope-origin ORIGIN …] [--data-root path] [--mcp-command path --mcp-sha256 sha256 --token-file path] [--once] [poll/backfill/tail limits]` | `G/cmd/bb-assistant/main.go:182`; `G/cmd/bb-assistant/caido_sync.go:26` |

Некоторые usage строки сокращают реальный набор flags: например `neo4j-backfill` поддерживает `--verify-only` (`main.go:298`), а `caido-tail/import/reconcile` — MCP binary/token/body bounds. Источник полной flag-спецификации — соответствующий `flag.NewFlagSet` в коде; универсальной REST-команды `scan`/`hunt` у Go CLI нет. `caido-sync` не имеет собственной literal `usage: …`, но явно есть в main usage/switch и реализует flags в `caido_sync.go:26–69`; это не повод исключать его из списка.

### 5.4. MCP `exploit_search` и backend'ы: что установлено, а что нет

Подтверждена **конфигурационная интеграция**, не реализация сервера:

```text
P/bb-hunter/config.yaml:29–44
MCP server key: exploit_search
transport: ssh -T … root@proxmox pct exec 108 python3
remote entrypoint:
/home/max/.local/share/bb-graph-assistant/mcp/exploit_search/server.py
```

Этот server key сам по себе не подтверждает имя конкретного MCP tool и его input schema. В `G/internal/mcpserver` нет такого инструмента; локального кода `exploit_search`/потребителей `VULNERS_*` в двух проверяемых репозиториях не найдено. У локального runtime root также не найден соответствующий `mcp/exploit_search/server.py`. Удалённый файл не читался: сетевые обращения для этой задачи запрещены.

**Следствие для ревью:** точные имена инструментов, backend list (Vulners/SearchSploit/ExploitDB/NVD и т.п.), fallback order, timeout/cache/rate limits, журналирование и точные `VULNERS_*` ENV **не подтверждены** и здесь не выдуманы. Нельзя считать backend `Vulners` действующим только из упоминания префикса в задании. Для завершения ревью этой внешней зависимости нужен локальный source snapshot remote `server.py` и его backend modules, redacted config и `tools/list` receipt. KB families `exploitdb`/`nvd` в manifest продукта — другая интеграция и не доказательство backend'ов exploit_search.

## 6. Конфигурация, окружение и deployment

### 6.1. ENV-контракт

Значения ключей/token/password намеренно не приводятся.

| Группа | Переменные и назначение | Источник |
|---|---|---|
| Private data/runtime | `BB_ASSISTANT_DATA_HOME`, `XDG_DATA_HOME`; launcher `BB_ASSISTANT_RUNTIME_ENV_FILE`, `BB_ASSISTANT_BINARY` | `G/internal/project/paths.go:20–48`; `G/scripts/bb-assistant:6–11,98–103` |
| Historical evidence | `BB_ASSISTANT_VAULT_KEY` — Base64URL 32-byte AES key; `BB_ASSISTANT_ALLOW_RAW_CAPTURE_TOOL=1` — sensitive MCP tool | `G/internal/ingest/vault.go:64–77`; `G/internal/mcpserver/server.go:178,1279–1288` |
| Graph | `BB_ASSISTANT_GRAPH_BACKEND=neo4j` default или explicit `sqlite`; `BB_ASSISTANT_NEO4J_URI/USERNAME/PASSWORD/DATABASE`; launcher `BB_ASSISTANT_NEO4J_ENV_FILE` | `G/internal/graphaccess/reader.go:141–147`; `G/internal/neo4jstore/store.go:71–74`; `G/scripts/bb-assistant:11` |
| Caido process | `CAIDO_URL`, `CAIDO_ACCESS_TOKEN`; `BB_ASSISTANT_CAIDO_MCP_COMMAND`, `…_MCP_SHA256`, `…_TOKEN_FILE` | `G/internal/caido/mcp_process.go:84,424–503` |
| Caido scope/instance | `BB_ASSISTANT_CAIDO_INSTANCE_ID`, `…_SCOPE_SCHEME/HOST/PORT`, `…_SCOPE_ORIGINS` | `G/cmd/bb-assistant/caido_sync.go:30–58`; `main.go:1386,1430–1431` |
| Legacy Caido | `BB_ASSISTANT_CAIDO_INSTANCE_KEY` используется Python tail template, а не Go sync `INSTANCE_ID`; child устанавливает `CAIDO_ALLOW_SENSITIVE_HEADERS=true` | `G/deploy/bb-assistant-caido-tail.service:15`; `G/internal/caido/mcp_process.go:503` |
| Gate LLM | `BB_ASSISTANT_LLM_URL/MODEL/API_KEY`; fallback `OPENAI_BASE_URL/OPENAI_MODEL/OPENAI_API_KEY` | `G/cmd/bb-assistant/gate.go:44–57`; `G/internal/gate/README.md:31–37` |
| KB runtime | `BB_ASSISTANT_KB_INDEX`, `…_SOURCE_MANIFEST`, `…_BACKEND=local\|redamon`, `…_PYTHON`, `…_MODELS` | `G/internal/kb/index.go:225–263`; `G/internal/mcpserver/server.go:130–218` |
| KB engines | `BB_ASSISTANT_KB_LEXICAL_ENGINE=sqlite-fts5\|neo4j-lucene`; Python `BB_ASSISTANT_KB_VECTOR_ENGINE=sqlite-cosine\|faiss` | `G/internal/kb/index.go:92–124`; `G/src/bb_assistant/kb/index.py:251–259` |
| Separate KB Neo4j | `BB_ASSISTANT_KB_NEO4J_URI/USERNAME/PASSWORD/DATABASE`, launcher `BB_ASSISTANT_KB_NEO4J_ENV_FILE` | `G/internal/kb/neo4j_lexical.go:60`; `G/scripts/bb-assistant:52–60` |
| RedAmon backend | `BB_ASSISTANT_REDAMON_ROOT`, `BB_ASSISTANT_REDAMON_KB_PATH` — Python pinned backend context | `G/src/bb_assistant/kb/model_worker.py:26,74` |
| Graph diagnostics | `BB_ASSISTANT_GRAPH_CONTINUATION_DIAGNOSTICS` — opt-in diagnostics, не readiness flag | `G/internal/appquery/graph.go:788` |
| Hunter integrations | `HUNTER_AGENT_RUNNER`, `HUNTER_RECON_RUNNER`, `BB_ASSISTANT_SOURCE`; optional `BB_ASSISTANT_API/PROJECT_KEY/SURFACE` | `H/sweep.sh:396–405`, `H/runners/README.md:38–40` |
| Hunter proxy/ledger | `HUNTER_GATEWAY_PORT` default 8090; `HUNTER_CAIDO_UPSTREAM` default `10.10.10.10:8080`; `HUNTER_PROXY/HUNTER_PROXY_URL`, HTTP(S)/ALL proxy ENV; `HUNTER_FINDINGS_LEDGER`, `HUNTER_CALLBACK_LOG` | `H/sweep.sh:16–25,348–366,589–600` |
| Hunter budget/governor | `HUNTER_SPECIALIST_BUDGET`, `HUNTER_SWEEP_DEADLINE_SECONDS`, `HUNTER_GOVERNOR_GLOBAL_RPS/HOST_RPS/STATE/LOG/DEGRADED` | `H/sweep.sh:206–214`; `H/verify/verify.py:162–167`; `H/governor.py:24–29` |
| External exploit lookup | Точные `VULNERS_*` не найдены в G/H; нельзя перечислить их как подтверждённый ENV API | Раздел 5.4 |

Go raw defaults и launcher defaults различаются: `scripts/bb-assistant` source-ит private env files, выбирает Python для моделей и по умолчанию Neo4j. Нельзя подменять фактический env работающего сервиса defaults из репозитория. Отдельный runtime file может содержать vault key; не публиковать его целиком.

### 6.2. Systemd: шаблоны не равны активным units

| Файл | ExecStart/назначение |
|---|---|
| `G/deploy/graph-kb-assistant.service:11` | User unit `scripts/bb-assistant serve --addr 127.0.0.1:8765`, без project keys — web/MCP без managed projection workers. |
| `G/deploy/graph-kb-assistant-system.service:8–21` | System user max, `/usr/local/bin/bb-assistant` через launcher; env из service.env; `serve … --project-key=${BB_ASSISTANT_PROJECT_KEY}`. |
| `G/deploy/graph-kb-assistant-kb-embed.service:10–25` | User max, Python KB embedding, offline model ENV, thread limits, thermal stop 80°C. Не следует запускать рядом второй embedder. |
| `G/systemd/graph-kb-assistant-caido-sync.service:14–17` | `/usr/local/bin/bb-assistant caido-sync`, private Neo4j/Caido env, 30 s poll, bounded backfill/tail, `--require-scope`; write root ограничен. |
| `G/systemd/bb-assistant-caido-tail@.service:10–15` | User bbassist, `/opt/.../bin/bb-assistant caido-tail --project-key %i … --require-scope --watch`, 10 s poll. |
| `G/systemd/bb-assistant-projection-worker@.service:10–14` | bbassist, per-project projection worker, watch 1 s, lease 10 m/8 attempts. |
| `G/systemd/bb-assistant-web-ui.service:8–12` | bbassist, standalone `web-ui --addr 127.0.0.1:8765` (без managed `/mcp`). |
| `G/deploy/bb-assistant-caido-tail.service:10–21` | **Legacy Python** `.venv/bin/bb-assistant-caido-tail`, `INSTANCE_KEY`, `import.env`; не тот же Go template. |

Активность units, выбранная версия deployed binary, реальные environment overrides и состояние CT108 **сегодня не опрашивались**. Несколько альтернативных templates в дереве не означают, что все они одновременно запущены.

### 6.3. Порты и trust boundaries

| Узел | Порт/интерфейс по коду или config | Неопределённость |
|---|---|---|
| Go serve/web-ui | `127.0.0.1:8765` default | Реальный bind может быть overridden; live socket list не снимался. |
| Nginx CT template | `80`, `8080`, Basic Auth + allow `192.168.1.0/24`, proxy → `127.0.0.1:8765` | `G/deploy/nginx/graph-kb-assistant.conf:6–25`; это HTTP, не TLS. Nginx 8080 и Caido 8080 — разные deployment nodes. |
| Caido | Standalone default `127.0.0.1:8080`; sweep upstream/gate `10.10.10.10:8080` | Нельзя переносить localhost между Kali и CT108. |
| Hunter gateway | `127.0.0.1:8090` default | `HUNTER_GATEWAY_PORT` меняет bind; proxy не target app API. |
| Neo4j graph | Example config `bolt://127.0.0.1:7687` | `G/deploy/neo4j/README.md:13`; фактический URI из private env, не установлен чтением template. |
| KB Neo4j | Отдельный `BB_ASSISTANT_KB_NEO4J_URI` | Может быть другой хост/порт; нет разрешения объявлять общую DB. |
| MCP bb_assistant/exploit_search | SSH stdio, не собственный HTTP listener | SSH endpoint `proxmox`; exploit command идёт в CT108. |

Nginx принудительно задаёт upstream Host и Origin loopback (`graph-kb-assistant.conf:21–22`); external auth/CSRF assumptions требуют отдельного ревью. Нет основания считать этот template безопасным public deployment; комментарий прямо запрещает public Internet exposure (`:1–4`).

## 7. Роли и барьеры безопасности/достоверности

### 7.1. Gate — readiness, fail-closed

`G/internal/gate` различает critical target/Caido/recon и noncritical optional LLM. `valid=0`, `degraded=2`, `invalid=3`; partial LLM config — degraded, полностью отсутствующая — skipped. Target probe: GET **origin `/`**, HTTP 200, text/html, body >1024 bytes. Caido: authenticated GraphQL `{projects{id name}}`, непустые проекты, без GraphQL errors. Recon default minimum 20 routes/5 input-points; повторяющиеся HTML hashes с combined share ≥50% блокируют SPA-shell inventory (`G/internal/gate/README.md:13–64`, `cmd/bb-assistant/gate.go:36–57`).

Это барьер пригодности данных, не policy для всех downstream requests и не proof of complete coverage. Он может отвергнуть здоровый API-only target. С другой стороны, достаточное число route rows без fingerprints не даёт семантической гарантии реальных distinct routes. Sweep требует именно valid exit 0; degraded также останавливает (`H/sweep.sh:310–311`).

### 7.2. Gateway — policy + журнал

HTTP checks: exact origin/optional slash-prefix, methods, destructive-path tokens, rolling origin RPS, Content-Length body cap, запрет chunked request body. Denials 403/429/413/400, upstream failure 502; denied HTTP не пересылается в Caido (`H/gateway/README.md:53–84`, `gateway.py:307–328`).

Journal содержит timestamp, request ID, method, **URL**, original Host, upstream, decision/reason/status, declared byte count, duration, body digest, client socket; body/Authorization headers не пишет. CONNECT пишет authority/byte counts/duration, не HTTP внутри TLS (`README.md:86–100`). Client socket — не OS user/process identity. Query secrets в URL могут попасть в journal; запрет sensitive query names есть у class context (`H/verify/verifiers/common.py:126–135`), но не является общим redactor для всех gateway clients.

### 7.3. Governor — RPS/breaker, не scope firewall

Cross-process state под `fcntl.flock`, rolling one-second admission: global 10/per-host 5 RPS defaults. После 3 последовательных timeout/5xx — pause 60 s + degraded marker; streak общий в shared state, не отдельный по host (`H/governor.py:24–29,82–174`). ffuf/feroxbuster/katana получают rate flags, scanner pause 2 s. Этот слой ограничивает вызывающих его клиентов/shims, но не произвольный network code и не число HTTP requests внутри browser workflow, где admission выдаётся на workflow (`H/verify/verify.py:159–178`).

### 7.4. Supervisor — obligations, не oracle

Hypothesis должна быть закрыта или иметь linked checks; последний inconclusive без повторного результата запрещает DONE; blocked check/hypothesis, незавершённая coverage, `calls_used >= max_calls` и прошедший UTC deadline → PARTIAL. Malformed/missing input → BLOCKED. Exit 0/2/3 (`H/supervisor/supervisor.py:21–77`). Evidence ref здесь проверяется как непустая строка, **не** читает файл/подпись (`H/supervisor/obligations.py:123–128`). Следовательно supervisor проверяет структуру декларации, а не истинность всех доказательств.

### 7.5. Матрица 38 классов и 38 plugins

Таблица перечисляет **все 38 registry classes** и реальный plugin. Registration: `H/verify/verifiers/__init__.py:23–33` + `extra.py:42–73`. Классовый verdict `PASS` означает положительный результат конкретного oracle, не «безопасно» и не автоматически bounty-ready. `FAIL` — данный сценарий не подтвердился, а не весь класс исключён.

Обозначения: **HTTP** — plugin вызывает bounded verification context; **browser** — нужен внешний adapter; **OOB** — нужен callback log; **fixture** — анализирует supplied `finding.verification[class]`, не собирает независимые данные. Наличие непустых `evidence_refs` в fixture не означает, что plugin проверил содержимое файла. Точный homonymous playbook может отсутствовать, даже если есть общая инструкция по parent class.

| Класс | Режим oracle | Смысл/основные prerequisites | Exact playbook | Реализация/registration |
|---|---|---|---|---|
| `xss` | browser | Harmless browser marker + clean baseline. | `playbooks/xss.md` | `H/verify/verifiers/xss_common.py:39`; registration `H/verify/verifiers/extra.py:66` |
| `xss-dom` | browser | DOM execution marker в browser, не HTML echo. | Нет homonymous файла | `H/verify/verifiers/xss_dom.py:6`; registration `H/verify/verifiers/__init__.py:27` |
| `xss-reflected` | browser | Reflected path: browser execution с чистым baseline. | Нет homonymous файла | `H/verify/verifiers/xss_reflected.py:6`; registration `H/verify/verifiers/__init__.py:26` |
| `xss-stored` | browser | Follow-up visit, execution + mutation opt-in для submit. | Нет homonymous файла | `H/verify/verifiers/xss_stored.py:6`; registration `H/verify/verifiers/__init__.py:28` |
| `xss-api-only` | fixture | API source→consumer sink/execution flags; без consumer trace BLOCKED. | Нет homonymous файла | `H/verify/verifiers/xss_api_only.py:10`; registration `H/verify/verifiers/extra.py:68` |
| `xss-csp-policy` | fixture | Captured CSP text: unsafe script directives. | `playbooks/xss-csp-policy.md` | `H/verify/verifiers/xss_csp_policy.py:12`; registration `H/verify/verifiers/extra.py:69` |
| `sqli` | HTTP | Три stable controls + три repeats задержки, distribution oracle. | `playbooks/sqli.md` | `H/verify/verifiers/sqli.py:20`; registration `H/verify/verifiers/__init__.py:24` |
| `nosql` | HTTP | True/false probes: complete одинаковой длины ответы, differential. | `playbooks/nosql.md` | `H/verify/verifiers/nosql.py:20`; registration `H/verify/verifiers/__init__.py:25` |
| `ssti` | fixture | Harmless expression/result; reflection не считается evaluation. | `playbooks/ssti.md` | `H/verify/verifiers/ssti.py:11`; registration `H/verify/verifiers/extra.py:43` |
| `xxe` | HTTP + OOB/marker | Unique callback либо synthetic file marker; callback reader обязателен. | `playbooks/xxe.md` | `H/verify/verifiers/xxe.py:37`; registration `H/verify/verifiers/extra.py:67` |
| `ssrf` | HTTP + OOB | Unique token, ровно одно callback-log event; read-only probe. | `playbooks/ssrf.md` | `H/verify/verifiers/ssrf.py:13`; registration `H/verify/verifiers/__init__.py:29` |
| `idor-bola` | HTTP | Owner B positive control против actor A; write с snapshot/rollback/readback. | `playbooks/idor-bola.md` | `H/verify/verifiers/idor_bola.py:50`; registration `H/verify/verifiers/__init__.py:30` |
| `bfla` | HTTP | B positive function control против A, одинаковый read-only request. | `playbooks/bfla.md` | `H/verify/verifiers/bfla.py:26`; registration `H/verify/verifiers/__init__.py:31` |
| `bopla` | fixture | Recorded property policy + actor-access facts. | `playbooks/bopla.md` | `H/verify/verifiers/bopla.py:10`; registration `H/verify/verifiers/extra.py:44` |
| `auth-session` | fixture | Recorded authenticated before/logout/after revocation. | `playbooks/auth-session.md` | `H/verify/verifiers/auth_session.py:10`; registration `H/verify/verifiers/extra.py:70` |
| `jwt-crypto` | fixture | Recorded alg=none/substituted signature protected-action acceptance. | `playbooks/jwt-crypto.md` | `H/verify/verifiers/jwt_crypto.py:12`; registration `H/verify/verifiers/extra.py:56` |
| `oauth-oidc` | fixture | Recorded callback/state misbinding flow evidence. | `playbooks/oauth-oidc.md` | `H/verify/verifiers/oauth_oidc.py:10`; registration `H/verify/verifiers/extra.py:45` |
| `mfa-2fa` | fixture | Recorded protected-action success без второго фактора. | Нет homonymous файла | `H/verify/verifiers/mfa_2fa.py:10`; registration `H/verify/verifiers/extra.py:46` |
| `password-reset` | fixture | Recorded reset-token single-use/replay evidence. | Нет homonymous файла | `H/verify/verifiers/password_reset.py:10`; registration `H/verify/verifiers/extra.py:47` |
| `business-logic` | fixture | Recorded invariant violation + readback/restore facts. | `playbooks/business-logic.md` | `H/verify/verifiers/business_logic.py:10`; registration `H/verify/verifiers/extra.py:71` |
| `race-condition` | fixture | Concurrent submissions и doubled readback effect facts. | Нет homonymous файла | `H/verify/verifiers/race_condition.py:10`; registration `H/verify/verifiers/extra.py:48` |
| `file-upload` | fixture | Benign fixture + exact retrieved-byte size/hash/policy. | `playbooks/file-upload.md` | `H/verify/verifiers/file_upload.py:10`; registration `H/verify/verifiers/extra.py:72` |
| `path-traversal` | fixture | Recorded out-of-root read и explicit synthetic marker. | `playbooks/path-traversal.md` | `H/verify/verifiers/path_traversal.py:11`; registration `H/verify/verifiers/extra.py:49` |
| `open-redirect` | fixture | Captured Location destination, не follow redirect. | `playbooks/open-redirect.md` | `H/verify/verifiers/open_redirect.py:22`; registration `H/verify/verifiers/extra.py:50` |
| `csrf` | fixture | Recorded cross-origin authenticated state-change evidence. | Нет homonymous файла | `H/verify/verifiers/csrf.py:10`; registration `H/verify/verifiers/extra.py:51` |
| `deserialization` | fixture | Safe fixture parsing/integrity behavior, без gadget-chain execution. | `playbooks/deserialization.md` | `H/verify/verifiers/deserialization.py:10`; registration `H/verify/verifiers/extra.py:52` |
| `info-disclosure` | fixture | Complete recorded response + sensitive-field/actor context. | Нет homonymous файла | `H/verify/verifiers/info_disclosure.py:10`; registration `H/verify/verifiers/extra.py:53` |
| `security-misconfig` | fixture | Headers против supplied required-header policy. | `playbooks/security-misconfig.md` | `H/verify/verifiers/security_misconfig.py:10`; registration `H/verify/verifiers/extra.py:54` |
| `crypto-weakness` | fixture | Observed algorithm против explicit allowlist. | `playbooks/crypto-weakness.md` | `H/verify/verifiers/crypto_weakness.py:10`; registration `H/verify/verifiers/extra.py:55` |
| `prompt-injection` | fixture | Independent model/tool trace; один output text не oracle. | `playbooks/prompt-injection.md` | `H/verify/verifiers/prompt_injection.py:10`; registration `H/verify/verifiers/extra.py:57` |
| `graphql-specific` | fixture | Schema-dependent fixture; disabled introspection может BLOCKED. | `playbooks/graphql-specific.md` | `H/verify/verifiers/graphql_specific.py:10`; registration `H/verify/verifiers/extra.py:58` |
| `supply-chain` | fixture | Recorded SBOM/artifact signature/provenance evidence. | `playbooks/supply-chain.md` | `H/verify/verifiers/supply_chain.py:10`; registration `H/verify/verifiers/extra.py:59` |
| `vulnerable-outdated-components` | fixture | Complete inventory + explicit advisory snapshot/version matching. | `playbooks/vulnerable-outdated-components.md` | `H/verify/verifiers/vulnerable_outdated_components.py:10`; registration `H/verify/verifiers/extra.py:60` |
| `unrestricted-resource-consumption` | fixture | Safe bounded quota/count fixture; unbounded или не exceeded quota BLOCKED. | `playbooks/unrestricted-resource-consumption.md` | `H/verify/verifiers/unrestricted_resource_consumption.py:10`; registration `H/verify/verifiers/extra.py:61` |
| `sensitive-business-flow-abuse` | fixture | Explicit business-flow policy + два test actors. | Нет homonymous файла | `H/verify/verifiers/sensitive_business_flow_abuse.py:10`; registration `H/verify/verifiers/extra.py:62` |
| `improper-api-inventory` | fixture | Observed routes против authoritative inventory. | `playbooks/improper-api-inventory.md` | `H/verify/verifiers/improper_api_inventory.py:10`; registration `H/verify/verifiers/extra.py:63` |
| `unsafe-api-consumption` | fixture | Recorded inert upstream fixture/trust-boundary behavior. | `playbooks/unsafe-api-consumption.md` | `H/verify/verifiers/unsafe_api_consumption.py:10`; registration `H/verify/verifiers/extra.py:64` |
| `security-logging-monitoring` | fixture | Injected fixture event + complete log/alert search facts. | `playbooks/security-logging-monitoring.md` | `H/verify/verifiers/security_logging_monitoring.py:10`; registration `H/verify/verifiers/extra.py:65` |

Число зарегистрированных plugins равно taxonomy (38), отсутствующих ключей при статическом сравнении нет. Но `H/CLASS-REGISTRY.json:60–68` всё ещё заявляет `covered=10/partial=28/no_verifier=28`; это **устаревшая оценка возможностей**, а не результат текущей проверки. Homonymous playbooks отсутствуют для `xss-dom`, `xss-reflected`, `xss-stored`, `xss-api-only`, `mfa-2fa`, `password-reset`, `race-condition`, `csrf`, `info-disclosure`, `sensitive-business-flow-abuse`; общие XSS/auth/business/ui-artifacts playbooks не равны explicit resolver для sweep.

Два уровня coverage нельзя смешивать: class registry описывает assets/capability; run coverage — фактические попытки/исходы. Go `CanFinish` проверяет declared budgets и untested positive-budget cells, допускает exhausted blocked budget; `Pct` считает только confirmed/ruled_out (`G/internal/coverage/matrix.go:81–109`). Shell добавляет более строгий запрет DONE при blocked_first_party, поэтому отдельный Go `can_finish=true` не тождествен итоговому sweep DONE.

## 8. Известные ограничения, расхождения и открытые дефекты

Ниже отделены текущие source-наблюдения от исторических утверждений. Они не исправлялись в рамках документа.

1. **Gateway не является единым обязательным egress-контуром.** HTTPS CONNECT непрозрачен: method/path/body/RPS внутри TLS не проверяются (`H/sweep.sh:327–340`, `gateway/README.md:46–51`). Кроме того, `hunter-recon-runner` фиксирует `PROXY="http://127.0.0.1:8080"`, строит explicit ProxyHandler и scanner flags из него (`:28,432,468,561–570`). Это обходит sweep gateway `8090` и может использовать другой Caido, чем `10.10.10.10`. Gate делает сеть ещё до gateway. Не утверждать «весь HTTP(S) проходит policy+journal».
2. **Нет сквозных 38 классов.** `hunter-agent-runner` triage и verifier registry знают 38, но `sweep.sh:150,410` и `sweep_coverage.py:11–14` оставляют 11. Прямой `--classes bfla` или другой новый класс не соответствует текущему shell allowlist; all не означает 38 сквозных specialists. `CLASS-REGISTRY.json` flags и ORCHESTRATOR «11 допустимых» устарели относительно plugins. Нужен единый authoritative class registry и тест согласованности всех consumers.
3. **Независимость verifier неоднородна.** Большая часть добавленных plugins принимает recorded booleans/counts/hashes/policy facts и не вызывает сеть/браузер/файлы (`H/verify/verifiers/_extra_common.py:1–4,30–44`). Например `xss-api-only` может PASS по flags source/sink/execution, `unrestricted-resource-consumption` — по supplied quota/counts (`xss_api_only.py:12–25`, `unrestricted_resource_consumption.py:12–28`). Это полезная типизированная оценка fixture, но не независимое воспроизведение. Непустой reference проверяется по форме, содержимое не аттестуется.
4. **XSS adapter не подключён к CLI sweep.** `verify.py --class-verifiers` не передаёт `browser_runner` (`:1068–1072`); XSS требует такой adapter. SSRF/XXE без callback reader/log BLOCKED; XXE file-read ветка тоже требует callback reader по контракту (`verify/verifiers/README.md:24,32,44`). «38 verifiers» не означает 38 работоспособных production oracles.
5. **Историческое «два класса были BLOCKED» не установлено как конкретный актуальный результат.** В двух репозиториях не найден immutable run receipt с парой классов и причинами, позволяющий воспроизвести это точное утверждение. В свежих локальных `hunter-runs/*20261030*` встречаются старые generic `manual/reproduced` artifacts, без class-verifier attribution; их нельзя приписать нынешним plugins. Подтверждены лишь условия BLOCKED из кода выше и в таблице. Нельзя смешивать их с **двумя dead-letter projection jobs** в старом CT108 status: это другой слой.
6. **Gate проверен по unit/mock контрактах, не доказан полный реальный проход в этом ревью.** `G/internal/gate/*_test.go` использует локальные fixture HTTP servers; `H/test_sweep_gate.py:12–125` подменяет/builds fixture assistant. Это подтверждает наличие тестового покрытия логики, не работу настоящего Caido и target. Тесты сейчас не запускались. Заявление «gate был проверен на моке» не следует расширять до «доказан live production gate».
7. **Staged recon и gate receipt не связаны с новым recon.** Gate проверяет копии до active recon, после замены нет повторного gate. Нет доказанной привязки staged files к exact target и текущему времени только из их числа (`H/sweep.sh:191–200,242–244,387–405`). Target probe также игнорирует заданный path-prefix и ходит в origin `/`; требование HTML >1 KiB делает gate непригодным для части API/subpath приложений (`G/internal/gate/README.md:25–26`).
8. **Auto identities — скрытая специализация и mutation boundary.** Каждый specialist при отсутствии общего файла делает регистрации/login по lab-specific endpoints вне проверки `--allow-mutations`; bootstrap `_opener` не отключает redirects (`H/runners/hunter-agent-runner:1065–1085`, `identities.py:22–24,38–71`). HTTP gateway без mutation flag заблокирует POST, а TLS tunnel не увидит его. Одновременные specialists могут соревноваться за один `identities.json` (existence check + non-atomic write без lock); написание секрета предшествует chmod. Это не универсальный двухакторный adapter.
9. **Фактические capabilities bb-hunter расходятся с SOUL/runner.** Config разрешает 15 MCP tools плюс skills/clarify/memory/exploit_search, но не terminal/browser/file. SOUL описывает их как разрешённые и specialist brief просит создавать evidence files. По одному SOUL нельзя заключить, что агент умеет выполнить brief. `--yolo` в subprocess не создаёт отсутствующие toolsets; профиль/провайдерные overrides требуют отдельного executable capability test, который здесь не выполнялся.
10. **Coverage до class verify и разные vocabularies.** Матрица использует specialist `validation=confirmed`, создаётся до verify; после plugin FAIL не обновляется автоматически (`H/sweep.sh:575–608`, `sweep_coverage.py:114–130`). Supervisor понимает PASS/FAIL и legacy reproduced/manual (`supervisor/build_obligations.py:77–88`), но ledger сохраняет verdict как есть (`ledger.py:248–260`). Report отдельно отображает class `PASS` как `reproduced`, `FAIL` как `not_reproduced`, прочие class verdicts как `manual`, сохраняя raw breakdown по verifier (`sweep_report.py:281–288`). Downstream должен учитывать происхождение статуса, а не объединять `confirmed`, `PASS`, `reproduced`, saved candidate и accepted proposal в одно поле истины.
11. **Ledger и run artifacts не transactional audit log.** Ledger хранит изменяемый current status и history, но физически переписывается без межпроцессного lock/atomic rename. Fingerprint не включает class, значит тот же request может слить разные class interpretations; отсутствие actor-контекста в canonical material требует отдельной оценки (`H/ledger.py:99–111,156–165,272–280`). Ledger обновлён до supervisor; PARTIAL run тоже может изменить реестр.
12. **Tool-call budgets имеют неполную наблюдаемость.** Supervisor считает gateway journal entries (`H/supervisor/build_obligations.py:225–258`), отчёт имеет отдельную эвристику specialist logs (`H/SWEEP.md:40`, `H/sweep_report.py`). CONNECT — одна journal entry на tunnel, не число вложенных запросов; direct/bypassed traffic не попадает в journal. Это не строгий общий сетевой или LLM budget. Governor state при повреждённом JSON сбрасывается в `{}` (`H/governor.py:89–94`), то есть limiter не везде fail-closed.
13. **Historical evidence и live graph асимметричны.** Live-tail даёт structural graph, но UI/consultation exact evidence требуют historical vault; missing response/truncation/reconstructed wire fidelity должны остаться видимыми. Correlation/hash integrity не доказывают причинность/уязвимость. Смена vault key меняет keyed signatures новых проекций; резервное копирование без ключа не даёт пригодного восстановления evidence.
14. **Несколько хранилищ и очередей требуют convergence proof.** Capture DB transaction, receipt/vault files, BehaviorGraph/outbox и Neo4j — не атомарный commit. Dead letters, partial source coverage и неочевидный владелец workers не устраняются успешной health-проверкой. Исторический status CT108 от 2026-10-26 сообщает 1400 captures, 1398 applied и 2 dead-letter; это датированное свидетельство, не текущий счётчик (`G/docs/PROJECT-STATUS.md:3418–3441`).
15. **KB candidate не promoted — подтверждено только датированным status.** Последняя запись 2026-10-26 описывает `kb/candidates/full-20261026-1936/index.sqlite3`: incomplete vectors, manifest mismatch, unconfirmed quick_check, thermal wait; active index оставлен прежним. Не утверждать, что candidate уже переключён или полностью проверен (`G/docs/PROJECT-STATUS.md:3443–3487`). Текущие remote процессы не проверялись. Exact RedAmon corpus/query parity и ranking identity остаются недоказанными по этому источнику; matching chunker/модели недостаточно.
16. **«NVD в очереди» неоднозначно и не подтверждено для текущего продукта.** Current `sources.yaml:56–70` уже включает NVD с reviewed terms; исторический active full snapshot содержал семь families (`G/docs/PROJECT-STATUS.md:3034–3062`). Если речь о будущем NVD backend exploit_search, его source не доступен (5.4); если об embedding candidate — incomplete vectors не равны отсутствию NVD corpus. Этот pending label нельзя переносить между системами без receipt.
17. **Внешний exploit_search остаётся непросмотренной dependency.** Local profile запускает SSH под `root@proxmox` и `pct exec 108`, тогда как app MCP использует отдельного restricted user. Backend source/credentials lifecycle/output redaction неизвестны. Требуется отдельный source+privilege review; отсутствие кода в G/H — конкретная граница документа, не утверждение о том, что сервера нет.
18. **HTTP exposure/auth boundary внешняя.** Go sensitive evidence и local state mutations доступны через loopback без встроенной user auth; Nginx template обеспечивает Basic Auth/IP ACL, но HTTP без TLS, и переписывает Origin. `/mcp` имеет отдельный SDK guard, не webui gate. Необходим threat model proxy+browser+local user, а не допущение «localhost безопасен» (`G/deploy/nginx/graph-kb-assistant.conf:1–25`, `G/internal/mcpserver/streamable_http.go:64–75`).

## 9. Как запустить и проверить

Команды ниже — **инструкция будущему оператору**, в рамках подготовки документа не выполнялись. Не запускать active строки без разрешения на конкретную цель и методы. Source требует Go **1.27.0** (`G/go.mod:3`); Python KB/model dependencies устанавливаются отдельно, gateway использует stdlib, browser/oracle adapters имеют свои зависимости.

### 9.1. Сборка и offline verification продукта

```bash
cd /home/max/bb/graph-kb-assistant
# $TMPDIR должен указывать на приватный scratch, не на каталог с исходниками/данными.
go build -o "$TMPDIR/bb-assistant" ./cmd/bb-assistant
"$TMPDIR/bb-assistant" version
"$TMPDIR/bb-assistant" mcp-tools
# Unit verification; integration/env flags перед этим проверить и не включать.
go test ./...
go vet ./...
```

Нет обещания, что эти команды уже PASS на данном checkout: в этом ревью они не запускались. `go test` не следует выполнять с сохранёнными opt-in Neo4j/browser integration ENV без отдельной проверки scope.

### 9.2. Продукт с существующим private runtime

```bash
cd /home/max/bb/graph-kb-assistant
./scripts/bb-assistant status
./scripts/bb-assistant projects
./scripts/bb-assistant neo4j-check
# Web + MCP, без автоматического Caido intake и без workers:
./scripts/bb-assistant serve --addr 127.0.0.1:8765
# Альтернатива, НЕ параллельно на том же порту: добавить выбранный worker.
./scripts/bb-assistant serve --addr 127.0.0.1:8765 --project-key "$PROJECT_KEY"
```

До запуска настроить private runtime.env/neo4j.env, selected data root и при необходимости KB index/model worker. `PROJECT_KEY` брать из registry, не выдумывать. Для диагностики source/queue использовать отдельные `project-audit`, `caido-reconcile`, `projection-reconcile` из таблицы CLI; `neo4j-check --ensure-schema`, restore/backfill/apply — изменения, не read-only readiness.

Пример чтения API после разрешённого запуска (не target request):

```bash
curl --fail --silent http://127.0.0.1:8765/api/health
curl --fail --silent --get http://127.0.0.1:8765/api/surface \
  --data-urlencode "project_key=$PROJECT_KEY" --data-urlencode 'limit=10'
# MCP stdio отдельно; launcher требует корректные private runtime settings:
./scripts/bb-assistant mcp-server
```

`caido-register` создаёт локальный project namespace; не переключает проект в Caido (`G/internal/store/project.go:107–109`). Intake service требует точный instance ID, MCP pin/hash/token и exact authorized origins. Не копировать credentials в командную строку/Markdown.

### 9.3. Hunter-tools: сначала план и локальные contracts

```bash
cd /home/max/bb/hunter-tools
./sweep.sh --help
./sweep.sh --target https://authorized.example --classes sqli,xss \
  --out "$TMPDIR/hunter-review-next"
# Только offline fixture-проверки, без recon/цели:
python3 -m pytest -q verify gateway/tests supervisor/tests
python3 -m unittest discover -s runners -p 'test_*.py'
```

Пока расхождения раздела 8 не разобраны, шаблон active команды не является production-ready runbook. После approval/scope, пригодного inventory, реального Caido auth, решения gateway bypass, lab identity adapter и verifier inputs:

```bash
export HUNTER_AGENT_RUNNER=/home/max/bb/hunter-tools/runners/hunter-agent-runner
export HUNTER_RECON_RUNNER=/home/max/bb/hunter-tools/runners/hunter-recon-runner
# АКТИВНО: создаёт файлы, проверяет сеть, запускает специалистов.
./sweep.sh --target https://authorized.example --classes sqli,xss \
  --out /home/max/bb/hunter-runs/approved-new-run --recon --execute
```

`authorized.example` — placeholder, не найденный или разрешённый target. Не добавлять `--allow-mutations` автоматически; он не исправляет implicit bootstrap/HTTPS visibility. Не считать `--classes all` проверкой всех 38. Для standalone supervisor можно проверить **уже существующие** artifacts локально:

```bash
python3 supervisor/supervisor.py check \
  --obligations "$RUN_DIR/obligations.json" --coverage "$RUN_DIR/coverage.json" --json
```

Это оценивает deadline на момент команды и может изменить вычисленный verdict без изменения target; не overwrite исходный исторический receipt, если нужна воспроизводимость. В acceptance внешнего ревью требуются: сопоставление inventory target/time, class consistency, actual egress path, independent oracle evidence, согласование coverage+verification+supervisor и проверка восстановления synthetic state.

## 10. Вопросы внешнему ревьюеру

1. **Единая network boundary:** должен ли gateway быть обязательным OS-enforced egress? Как покрыть gate, fixed recon proxy, bootstrap, browser, arbitrary child process и CONNECT без ложного заявления о path/method/RPS control?
2. **Единая taxonomy:** какой файл является authoritative для 38 классов? Как синхронизировать shell allowlists, coverage builder, runner signals, plugin registry, playbook resolver и docs, чтобы `all` имело проверяемый смысл?
3. **Достоверность plugins:** где проходит граница между model-supplied observation, fixture validation и independently re-fetched oracle? Можно ли получить PASS, только передав booleans и существующий reference, не подтвердив причинность и provenance?
4. **Completion semantics:** зачем coverage создаётся до verify? Как negative/inconclusive результат меняет confirmed cell? Не являются ли can_finish, DONE и coverage_pct разными метриками, которые UI/report сейчас смешивают?
5. **Privileges и capabilities:** почему specialist использует bb-hunter с ограниченным toolset и `--yolo`, а exploit_search — SSH root/pct exec? Какова минимальная действительная роль каждого и кто создаёт evidence?
6. **Identity lifecycle:** можно ли полностью вынести bootstrap из specialist, требовать explicit synthetic-state permission, adapters по target, no-redirect policy, atomic secret storage и cleanup/readback? Кто владеет accounts при параллельных specialists?
7. **Durable convergence:** доказан ли recovery для crash между vault/receipt, capture commit, graph import, outbox publish и job ack? Как конфликт revision и dead-letter отражается пользователю без ручного анализа SQLite?
8. **Ownership workers:** какой режим выбран — managed serve, standalone projection-worker, caido-sync? Как исключаются лишние writers и долговременная блокировка query/intake одной maintenance lease?
9. **Evidence boundary:** как не выдавать live structural observation за доступную exact historical response? Как обозначать реконструированные wire bytes, truncation, response refresh и отсутствие исходной версии?
10. **Secrets и redaction:** какие угрозы остаются от URLs/query в gateway journal, raw `/api/evidence`, identities/bootstrap, operator-reviewed excerpts и KB prompt injection? Проверены ли Nginx Origin rewrite и различие `/mcp`/UI guards?
11. **Ledger:** нужен ли append-only transactional store, origin/actor/scenario-aware fingerprint и отдельные поля class/status provenance? Как предотвратить lost updates и смешение двух классов одинаковым request fingerprint?
12. **KB promotion:** какие immutable snapshot/query receipts нужны до candidate promotion? Как отдельно измеряются source terms/freshness, corpus identity, vector completeness, lexical/rerank parity и thermal-safe maintenance?
13. **External exploit lookup:** где исходники/dependency pins/backend contract remote exploit_search, какие exact `VULNERS_*` используются и какие данные уходят во внешние сервисы? Может ли этот компонент незаметно обойти scope/redaction?
14. **Run reproducibility:** где versioned run manifest с commits, profile/tool allowlist, binary hashes, target scope, policy hash, model/provider, class registry version, ledger baseline и oracle receipts? Что позволяет другому агенту отличить старый generic результат от текущего class-specific?
15. **Тесты против реального acceptance:** какие утверждения доказаны mocks, какие synthetic local integration, какие авторизованным end-to-end? Какая минимальная проверка требуется, чтобы не объявлять readiness по одной health-ручке?

**Приоритет трёх спорных мест:** (1) фактический egress и непрозрачный HTTPS против обещаний gateway; (2) 38-class registry/plugins против 11-class sweep/coverage; (3) независимость evidence oracle и связь class verification с coverage/ledger/DONE. Это вопросы к архитектурным гарантиям, а не утверждение об эксплуатации цели.
