# План подготовки профиля `bb-hunter`

**Статус:** инвентаризация и проект решения; настройки не применялись. `config.yaml` и `SOUL.md` не изменялись. MCP-сервер не запускался и его live `tools/list` не запрашивался: настроенный launcher открывает SSH-сессию к `proxmox`, а доступ к `192.168.1.149` запрещён условиями инвентаризации.

## Что есть сейчас

### Профиль и toolsets

| Область | Наблюдаемое состояние |
|---|---|
| Профиль | `/home/max/.hermes/profiles/bb-hunter/` |
| Основная модель | `model.provider: openai-codex`, `model.default: gpt-6-luna`, `base_url: https://chatgpt.com/backend-api/codex`; `agent.reasoning_effort: high` |
| Верхнеуровневый `toolsets` | `mcp-bb_assistant`, `skills`, `clarify`, `memory` |
| CLI `platform_toolsets.cli` | Явный список: `clarify`, `memory`, `session_search`, `skills`, `todo`, `vision`. В установленном Hermes явный список платформы задаёт выбор её toolsets; активный MCP-сервер добавляется отдельно, если не отключён через `no_mcp`/фильтр. В `bb-hunter` такого отключения нет. |
| Доступный CLI-инструментарий по конфигурации | Встроенные toolsets из явного списка выше и MCP `bb_assistant`. В явном CLI-списке нет `delegation`, `terminal`, `browser`, `web`, `file`, `code_execution`, `computer_use`. `agent.disabled_toolsets` не задан, то есть отдельного глобального deny-list пока нет. `known_builtin_toolsets.cli` — каталог известных toolsets, не список включённых. |
| Делегирование | В `bb-hunter/config.yaml` нет секции `delegation`; `delegation` не включён в `platform_toolsets.cli`. Нет настроенного per-task model routing. `plugins.enabled` в профиле также не задан. |
| MCP | `mcp_servers.bb_assistant.enabled: true`, `trust: full`, command `/home/max/.local/bin/bb-assistant-mcp-pve`. Сам launcher — shell-обёртка, выполняющая `ssh -T ... bb-assistant-mcp@proxmox`; это не локальный stdio-сервер. В профиле нет `mcp_servers.bb_assistant.tools.include` — Hermes не имеет явного клиентского allowlist имён инструментов. |
| Skills | `skills.external_dirs` содержит только `/home/max/bb/graph-kb-assistant/.claude/skills`; по нему доступно 6 проектных скиллов. Каталог `/home/max/bb/.claude/skills` (41 скилл) пока не подключён. `skills.write_approval: true`, `skills.guard_agent_created: true`. |

### Проектные скиллы — 6

| Скилл | Назначение |
|---|---|
| `caido-history-graph` | Read-only intake и поиск Caido history, per-project граф, provenance и coverage; прямо запрещает обращаться к целям. |
| `evidence-triage-lifecycle` | Разделяет наблюдения, модельные оценки, гипотезы и подтверждённые находки; запрещает продвигать кандидата по одному лишь score/графу/KB. |
| `graph-guided-vulnerability-analysis` | Анализ уже собранных данных через BehaviorGraph и KB с точными ссылками на доказательства; формирует гипотезу, не выполняет активную проверку. |
| `harness-provider-evaluation` | Сравнение agent harness и model provider для standalone bug-bounty assistant. Инженерный, не охотничий сценарий. |
| `kb-source-rebuild` | Пересборка/обновление KB из закреплённых источников с provenance и контролем покрытия; операция обслуживания, не охота. |
| `mcp-readonly-tool-design` | Принципы узкого read-only MCP фасада, схем, лимитов, provenance и редактирования чувствительных данных; отдельно отмечает, что consultation — запись состояния. |

### Пользовательские скиллы `/home/max/bb/.claude/skills` — 41

В таблице — имя каталога/скилла и краткое назначение из `SKILL.md` frontmatter `description`. Многие из этих скиллов предназначены для активной разведки/проверок; подключение каталога целиком не рекомендуется.

| Скилл | Назначение |
|---|---|
| `agents-best-practices` | Проектирование, аудит и улучшение агентных harness/архитектур. |
| `android-malware-jadx` | Анализ вредоносного Android APK через JADX, включая сбор данных, C2 и эскалацию привилегий. |
| `android-reverse-engineering` | Декомпиляция Android/JAR/AAR и поиск API/вызовов в приложении. |
| `api-caido-investigation` | Инвентаризация API и ограниченные проверки через JavaScript, browser и Caido; допускает replay/проверки при авторизации. |
| `apktool-re` | Распаковка APK, ресурсов, manifest и smali через apktool. |
| `application-map` | Evidence-linked карта функций, UI, JS endpoints, акторов, объектов и наблюдаемого поведения; workflow включает browser-прогулку. |
| `auth-boundary-assist` | Разбор account takeover и нарушений границ авторизации/tenant/role/session. |
| `bitrix-pentest` | Проверки безопасности 1C-Bitrix, включая auth bypass и построение RCE-цепочек. |
| `bizone-monitor` | Мониторинг новых BI.ZONE bounty-программ и изменений scope. |
| `browser-app-map` | Read-only browser-разведка авторизованного приложения через закреплённый Caido proxy. |
| `bug-bounty-workflow` | Полный workflow bug bounty от разведки до триажа/отчёта; не ограничен только пассивным анализом. |
| `cms-check` | Определение CMS и построение приоритетного чек-листа известных проверок/CVE. |
| `codex-orchestrator` | Координация задач в workspace через Codex subagents. |
| `cve-matcher` | Сопоставление техстека с CVE, PoC, CISA KEV, EPSS и Nuclei-шаблонами. |
| `ghidra-ida-re` | Реверс-инжиниринг бинарников через Ghidra/IDA. |
| `h1-recon` | Поиск опубликованных HackerOne reports по техстеку/классу уязвимости. |
| `hist-dns` | Исторический DNS и поиск stale IP; workflow также может проверять найденные адреса через Caido. |
| `host-recon` | Инвентаризация авторизованного web host: технологии, routes, assets, headers и функции; включает активные этапы. |
| `hunt-log` | Локальная память охоты: hypotheses/findings, evidence validation, lifecycle, метрики; команды пишут локальные файлы. |
| `idor-assist` | Анализ IDOR/BOLA кандидатов по output других reconnaissance/traffic-скиллов. |
| `jenkins-pentest` | Проверки Jenkins, включая anonymous access и известные уязвимости/RCE-цепочки. |
| `js-endpoint-mapper` | Классификация endpoint-ов, найденных в JavaScript, по auth/admin/public/unknown и приоритету. |
| `js-recon` | Инвентаризация routes/API, упомянутых в JS авторизованного приложения. |
| `kb-grounding` | Привязка гипотез к локальной KB, источникам и условиям применимости; текущие имена ожидаемых KB-инструментов отличаются от `bb_assistant`. |
| `kibana-grafana-pentest` | Проверки Kibana/Grafana, misconfiguration и известных CVE. |
| `mass-fuzz` | Явно запрашиваемый активный массовый web content fuzzing по нескольким хостам. |
| `new-target` | Полная первичная оценка нового авторизованного приложения и coverage поверхности. |
| `offline-evidence-analysis` | Сравнение BB/PentestAgent по одному hash-pinned пакету сохранённых redacted Caido-запросов; запрещает сбор трафика и выполнение проверок. |
| `param-extractor` | Извлечение параметров endpoint-ов из Caido traffic для wordlist/ручной проверки. |
| `passive-recon` | OSINT и пассивное перечисление поверхности организации; обращается к внешним источникам. |
| `port-scan` | Активное сканирование masscan/nmap. |
| `prompt-validator` | Документация prompt-validation hook и правил workspace. |
| `react-vuln-checks` | Проверка React/Next.js приложения правилами semgrep и runtime-проверками. |
| `report-writer` | Форматирование уже воспроизведённой находки в отчёт BI.ZONE; не предназначен для стадии поиска. |
| `response-anomaly` | Ранжирование выбросов среди ответов из собранного Caido request set; есть отдельный активный режим. |
| `secret-chain` | Проверка привилегий найденного API token/secret и переходов к другим сервисам. |
| `tg-notify` | Отправка Telegram-уведомления (внешний side effect). |
| `traffic-triage` | Классификация уже собранного Caido traffic по потенциальным классам атак; скрипт использует Caido GraphQL. |
| `vhost-probe` | Активное обнаружение скрытых virtual hosts через HTTP Host header. |
| `web-fuzz` | Активный ffuf content/directory fuzzing по указанному URL. |
| `web-investigation` | Ограниченное расследование приложения от inventory до воспроизводимого evidence; включает проверки/запросы. |

### Проверка external-подкаталогов

Не найдены `/home/max/bb/.claude/skills/external`, `/home/max/bb/.claude/external-skills`, `/home/max/bb/.claude/skills-external`, `/home/max/bb/.claude/external` или `/home/max/bb/external-skills`. Фактическое число `SKILL.md`: проект — **6**, пользовательский каталог — **41**, всего — **47**.

### MCP `bb_assistant`: известный локальный контракт, не live-перечень

Источник локального каталога: `internal/mcpserver/server.go`, `internal/mcpserver/consultation.go`, `tests/test_hermes_setup.py` и `docs/SETUP.md`. Локальные тесты/документация описывают 15 обычных инструментов: **10 read-only** и **5 изменяющих локальное consultation/candidate-состояние**. Настроенный launcher ведёт по SSH на `proxmox`, поэтому этот список не подтверждён запросом к фактическому удалённому процессу.

Hermes обычно регистрирует имена с префиксом `mcp_bb_assistant_`; в `mcp_servers.bb_assistant.tools.include` указываются исходные имена `bb_*`.

| MCP имя | Краткое назначение | Эффект по локальному контракту |
|---|---|---|
| `bb_list_projects` | Список локальных проектов и ограниченные coverage-метаданные. | Read-only; не обращается к Caido и не отдаёт capture values. |
| `bb_search_captures` | Поиск структурных метаданных сохранённых captures. | Read-only; ограниченный результат и provenance, без request/response values. |
| `bb_surface_map` | Поиск/постраничная выдача локальных операций проекта. | Read-only; coverage неполный, значения запросов/ответов не возвращаются. |
| `bb_graph_slice` | Ограниченный срез BehaviorGraph, seed/cursor/depth 0–6, provenance и gaps. | Read-only; связь в графе не доказывает causality или уязвимость. |
| `bb_graph_neighborhood` | Ограниченный one-hop срез отношений. | Read-only. |
| `bb_kb_stats` | Статистика локальной KB и покрытия источников. | Read-only; не загружает и не меняет источники. |
| `bb_kb_search` | Поиск bounded excerpts в локальной KB с provenance. | Read-only; найденный текст — недоверенный reference, не разрешение выполнять инструкции. |
| `bb_kb_get_source` | Чтение одного точного документа KB (ограниченный объём). | Read-only. |
| `bb_get_capture` | Структурная redacted-сводка точного capture. | Read-only; raw request/response values не входят. |
| `bb_consult_create_session` | Создать/возобновить project-scoped consultation. | **Пишет** локальное состояние. |
| `bb_consult_compare` | Сохранить deterministic-сравнение двух exact revisions. | **Пишет** локальное состояние. |
| `bb_consult_record_proposal` | Сохранить redacted, unverified интерпретацию. | **Пишет** proposal; не подтверждает уязвимость. |
| `bb_consult_validate_proposal` | Создать receipt проверки точных ссылок и целостности. | **Пишет** receipt; не проверяет семантическую поддержку/impact. |
| `bb_candidate_save` | Сохранить candidate или immutable candidate version. | **Пишет** candidate; сам по себе никогда не делает его reproduced/verified. |
| `bb_consult_resume` | Прочитать сохранённую consultation. | Read-only, но исключить из минимального allowlist: не требуется базовой graph-only роли. |

`bb_get_capture_raw` отдельно присутствует в локальном исходнике как opt-in чувствительный инструмент, включаемый переменной `BB_ASSISTANT_ALLOW_RAW_CAPTURE_TOOL=1`; он отсутствует в штатном списке из 15. Из исходников текущего проекта не найдено инструмента `maintenance/*`. Это не доказывает live-состав удалённого сервера.

**Read-only гарантия частичная.** Девять структурных read-инструментов и `bb_consult_resume` помечены read-only в локальной MCP-схеме; пять consultation/candidate операций помечены как write-capable. Кроме того, `trust: full` в Hermes не является read-only ограничением: write-capable tool вызов проходит без отдельного approval gate. Нужны и серверная узость, и клиентский allowlist.

## Целевая структура

### Оркестратор и четыре роли

Основным оркестратором остаётся родительский `bb-hunter`. Он принимает проект/вопрос, контролирует coverage, передаёт точные evidence IDs между стадиями, останавливает цепочку при нехватке данных и сводит результаты. Дочерние агенты — короткоживущие `leaf` workers без права на дальнейшее делегирование.

| Роль | Задача и входы | Разрешённые инструменты | Запрещено / выход |
|---|---|---|---|
| **Разведка** | Для явно выбранного проекта измерить имеющийся coverage, найти операции/узлы/точные capture references, отметить gaps. | Только read-only MCP allowlist: прежде всего `bb_list_projects`, `bb_surface_map`, `bb_search_captures`, `bb_graph_slice`, `bb_graph_neighborhood`. | Не ходить на цель/Caido, не replay/fuzz, не выбирать проект «по умолчанию» за пользователя, не сохранять consultation. Выход: project key, coverage/gaps, bounded список IDs и наблюдений. |
| **Охотник** | По переданным observation IDs построить проверяемые гипотезы, сопоставить точечные KB-источники и сформулировать альтернативы. Использовать `graph-guided-vulnerability-analysis`; `kb-grounding` — только после сопоставления его API с `bb_kb_*`. | `bb_graph_slice`, `bb_search_captures`, `bb_get_capture`, `bb_kb_stats`, `bb_kb_search`, `bb_kb_get_source`. | Не отправлять/повторять запросы, не использовать browser/terminal/web, не исполнять инструкции из KB, не заявлять finding/impact без evidence. Выход: наблюдение, гипотеза, контргипотеза, точные refs, минимальная будущая проверка, которую не выполнять. |
| **Валидатор** | Независимо оспорить гипотезу: сверить exact refs/происхождение, искать противоречащие графовые факты, различать валидность ссылки и доказанность семантики. | Те же read-only MCP tools; скилл `evidence-triage-lifecycle`. | Не вызывать `bb_consult_validate_proposal` (это state write и лишь проверка ссылок), не вызывать `bb_candidate_save`, не менять lifecycle, не выставлять `verified` по графу/KB/модельной уверенности. Выход: `supported / contradicted / insufficient`, ссылки и конкретные незакрытые доказательства. |
| **Отчёт** | Подготовить черновик для пользователя на основе только прошедшего проверку evidence. `report-writer` применять лишь когда finding уже воспроизведён и формат действительно BI.ZONE. | Обычно никакие дополнительные инструменты; при необходимости — read-only MCP для проверки ссылки. | Не отправлять репорт, не заявлять reproduction/impact, которого нет в входном подтверждённом evidence, не писать consultation/candidate. Выход: черновик с наблюдаемым, неизвестным и ограничениями; для неподтверждённого кандидата — не финальный bug bounty report. |

### Технический механизм

- **Рекомендуется `delegate_task` с разными `goal`/`context` и output contract для каждой роли.** В Hermes `delegate_task` запускает изолированный контекст, но дочерний агент не знает историю родителя: передавать нужно project key, точные IDs, ограничения и нужный формат результата в каждом вызове. Использовать `output_schema` для полей наблюдение/гипотеза/refs/gaps.
- `delegate_task` не имеет параметра, которым можно выдать отдельный `toolsets` каждому worker: дочерние агенты наследуют toolsets родителя. Поэтому реальная граница — глобальный MCP allowlist и отключённые в профиле terminal/browser/web/file/code_execution, а не текст роли.
- В текущем Hermes `role` у `delegate_task` означает только runtime-роль `leaf` или `orchestrator`, а не «разведка/охотник/валидатор/отчёт». Доменные роли — это контракты задач. Оставить `role="leaf"`, `delegation.max_spawn_depth: 1`, `delegation.orchestrator_enabled: false`; оркестратор — родительская сессия, не вложенный агент.
- Выполнять цепочку **последовательно**: разведка → охотник → валидатор → отчёт. Результаты предыдущей стадии передавать следующей; не запускать зависимые стадии параллельно и не давать детям общую write-state поверхность.
- `SOUL.md` задаёт идентичность основного агента, но не является хорошим механизмом переключения доменной роли. Более того, локальная документация Hermes говорит, что `SOUL.md` исключён из system prompt subagent. Оставить там общий запрет активных запросов и правила доказательности; роль и её точные ограничения передавать в каждом `delegate_task`.
- Отдельные профили уместны только если ролям нужны разные модели/skills/terminal policy или доверительные границы. Но текущая документация Hermes уточняет: `profile_routes` выбирает профиль для child, но child всё равно сохраняет toolset родителя. Для настоящей изоляции разных разрешений нужны отдельные запускаемые Hermes-профили/процессы с собственными MCP allowlist и системными ограничениями, а не только profile-route.

### Предлагаемая маршрутизация моделей

Текущий `bb-hunter` остаётся на `openai-codex / gpt-6-luna`. В профиле `oh-my-hermes-agent` уже описан рабочий образец `model_map` с вариантами `fast` (`gpt-6-luna`), `deep` (`gpt-6.1-sol`, `reasoning_effort: high`) и `review` (`gpt-6-astra`, `reasoning_effort: high`). Его можно перенести как конфигурационный образец после проверки доступности моделей и auth именно в `bb-hunter` — наличие маршрута в другом профиле не доказывает наличие его credentials здесь.

Предлагаемое соответствие: разведка → `fast`; охотник → `deep`; валидатор → `review`; отчёт → `deep` или `review` по уровню риска. Выбор опции делает родитель при каждом `delegate_task`; это не автоматическая роль-модель маршрутизация. `advanced-delegation` нужен для `delegation.backend: model_map` и должен быть добавлен в `plugins.enabled` профиля.

## Как подключить

Ниже — целевое состояние для будущего применения, не текущие изменения. Не заменять весь конфиг: сохранить текущие настройки модели и настройки skills, менять только перечисленные ключи. Применять после отдельного подтверждения.

### 1. Сначала сделать бэкап

Скопировать `/home/max/.hermes/profiles/bb-hunter/config.yaml` в соседний файл с уникальной датой/временем, сохранив owner и permissions (`cp -a`). Проверить, что копия создана и YAML разбирается. `SOUL.md` не трогать в рамках этого плана.

### 2. Ограничить MCP до read-only набора

Сначала сузить инструменты и trust, только потом включать новые возможности:

```yaml
mcp_servers:
  bb_assistant:
    command: /home/max/.local/bin/bb-assistant-mcp-pve
    enabled: true
    trust: untrusted
    tools:
      include:
        - bb_list_projects
        - bb_search_captures
        - bb_surface_map
        - bb_graph_neighborhood
        - bb_graph_slice
        - bb_kb_stats
        - bb_kb_search
        - bb_kb_get_source
        - bb_get_capture
    resources: false
    prompts: false
```

`tools.include` — whitelist исходных имён инструментов сервера. Из allowlist намеренно исключены все `bb_consult_*`, включая read-only `bb_consult_resume`, `bb_candidate_save` и опциональный `bb_get_capture_raw`. Перед применением следует сверить поля `resources/prompts` с используемой версией Hermes и не подменять существующий SSH command.

`trust: untrusted` добавляет approval gate для write-capable MCP вызовов без `readOnlyHint: true`, но не заменяет allowlist. Если сервер удалённо меняет инструменты/аннотации, проверять discovery заново.

### 3. Подключить только нужные user skills

Текущий проектный каталог уже подключён. Не добавлять целиком `/home/max/bb/.claude/skills`: Hermes рекурсивно сканирует `SKILL.md` в `external_dirs`, а это откроет все 41 скилл. Вместо этого добавлять отдельные каталоги с выбранными скиллами. Например:

```yaml
skills:
  external_dirs:
    - /home/max/bb/graph-kb-assistant/.claude/skills
    - /home/max/bb/.claude/skills/offline-evidence-analysis
    - /home/max/bb/.claude/skills/report-writer
  write_approval: true
  guard_agent_created: true
```

`report-writer` использовать только после независимого подтверждения и только для подходящего шаблона. `offline-evidence-analysis` — опционально, когда пользователь предоставил сохранённый redacted artifact.

`kb-grounding` полезен по смыслу, но в его текущей инструкции ожидаются `kb_stats()`, `kb_search()`, `kb_get_file()` и CLI-команды `hunt-log`; текущий MCP предлагает `bb_kb_stats`, `bb_kb_search`, `bb_kb_get_source`. Подключать только после адаптации skill-инструкции/alias mapping, исключив CLI-записи и активные проверки. Аналогично, `traffic-triage` не подключать без адаптации: его скрипт использует Caido GraphQL, тогда как профиль опирается на graph facade.

Project root уже содержит все 6 проектных скиллов. В охотничьем ходе использовать `graph-guided-vulnerability-analysis` и `evidence-triage-lifecycle`; остальные инженерные скиллы не запускать как hunt workflow. Если нужно уменьшить skill catalog, заменить root в `external_dirs` на отдельные каталоги этих скиллов и `mcp-readonly-tool-design`.

Так как внешние skill directories не являются filesystem read-only boundary, сохранить `skills.write_approval: true`; для жёсткой защиты использовать отдельную read-only копию/права на curated каталог, а не считать `external_dirs` защитой от записи.

### 4. Включить делегирование узко и плоско

Сохранить основной `model` как есть. Добавить `delegation` в явный CLI список, сохранив текущие нужные элементы и указав MCP явно; `agent.disabled_toolsets` использовать как дополнительный глобальный предохранитель. Пример целевых ключей (значения должны быть добавлены к существующим спискам, не перезаписывая неизвестные пользовательские настройки):

```yaml
platform_toolsets:
  cli:
    - clarify
    - memory
    - session_search
    - skills
    - todo
    - delegation
    - mcp-bb_assistant

agent:
  reasoning_effort: high
  disabled_toolsets:
    - terminal
    - browser
    - web
    - file
    - code_execution
    - computer_use
    - vision

delegation:
  backend: model_map
  max_concurrent_children: 1
  max_spawn_depth: 1
  orchestrator_enabled: false
  backends:
    model_map:
      default: fast
      options:
        - name: fast
          description: Быстрый сбор фактов из локального графа/KB.
          provider: openai-codex
          model: gpt-6-luna
        - name: deep
          description: Аккуратная формулировка гипотезы только по переданным доказательствам.
          provider: openai-codex
          model: gpt-6.1-sol
          reasoning_effort: high
        - name: review
          description: Независимая проверка доказательств и альтернатив.
          provider: openai-codex
          model: gpt-6-astra
          reasoning_effort: high

plugins:
  enabled:
    - advanced-delegation
```

Если у профиля уже появится `plugins.enabled` или `agent.disabled_toolsets`, объединить значения. Не включать `terminal`, `browser`, `web`, `file` или `code_execution` «для удобства» в рамках пассивного режима. `max_concurrent_children: 1` соответствует последовательной цепочке и сокращает риск параллельного изменения состояния; nested delegation остаётся закрытым.

### 5. Новая сессия и безопасная проверка после одобрения

После отдельного разрешения на применение: проверить config diff/валидность и resolved `platform_toolsets.cli`, `delegation` и `mcp_servers.bb_assistant.tools.include`; открыть новую Hermes-сессию, так как tool schema/подключение MCP строятся при старте сессии/процесса. Сначала подтвердить только список зарегистрированных MCP-инструментов (`tools/list`) и убедиться, что присутствуют ровно девять разрешённых read-only имён, а `consult_*`, `candidate_save`, `get_capture_raw` отсутствуют. Live discovery требует запуска SSH launcher; выполнить его можно лишь после явного подтверждения допустимого удалённого хоста и сетевого пути. В этой инвентаризации discovery не запускался.

Затем отдельным безопасным smoke-test запросить только локальную coverage-метрику, проверить, что write tool блокируется/не зарегистрирован, и вручную посмотреть фактическую модель/route child-результата. Не считать успешную конфигурацию доказательством валидности гипотезы; никакие тестовые обращения к цели в проверку не включать.

## Что сломается / риски

1. **Включение `terminal`, `browser` или `web` расширит реальные полномочия.** Скиллы из пользовательского каталога содержат `curl`, Caido, browser и активные тесты; одних запретов в prompt/SOUL недостаточно, особенно для child-процессов. В целевой конфигурации они остаются выключенными.
2. **Подключение всего пользовательского каталога добавит 41 скилл, среди них port-scan, mass/web-fuzz, vhost-probe, web-investigation, secret-chain и target-pentest workflows.** Это увеличивает риск неправильной маршрутизации, использования несовместимых CLI и нарушения ограничения «только сохранённые данные». Подключать отдельные skill-папки.
3. **`trust: full` не означает read-only.** Пять локально задокументированных MCP-инструментов изменяют consultation/candidate state и при `full` не требуют отдельного trust approval. Клиентский allowlist и `trust: untrusted` должны применяться вместе.
4. **Удалённый tool catalog не подтверждён.** Launcher ходит на SSH alias `proxmox`; local repo tests/docs не доказывают состав или версию живого сервера. В текущей работе `tools/list` и `192.168.1.149` не трогались. До live-discovery неизвестно, совпадает ли удалённый каталог с локальными 15 инструментами.
5. **Opt-in raw capture surface.** `bb_get_capture_raw` может появиться при включённой на сервере переменной `BB_ASSISTANT_ALLOW_RAW_CAPTURE_TOOL=1`; без явного `tools.include` это нельзя исключить на уровне profile. Даже redacted outputs и KB excerpts могут содержать чувствительную/недоверенную информацию.
6. **Валидатор не может доказать то, чего нет в MCP-выдаче.** Локальный контракт возвращает структурные/редактированные данные, без raw request/response values; coverage частичный. Проверка provenance и точных ID не равна семантической верификации, impact или воспроизведению.
7. **`delegate_task` — не sandbox по ролям.** Все дети наследуют инструменты родителя; отдельные prompt-ы лишь задают поведение. `SOUL.md` не передаётся ребёнку. Сильное ограничение обеспечивается MCP allowlist/toolsets, а для разных прав — отдельными процессами/профилями с собственными границами.
8. **Model-map может не заработать из-за auth/версии.** Конфигурация `oh-my-hermes-agent` — только локальный пример; его credentials и доступность моделей не проверялись для `bb-hunter`. Неудача выбранного backend должна fail closed; нельзя считать маршрут активным без результата тестового child.
9. **Внешние skills можно менять.** Hermes-документация предупреждает, что `skills.external_dirs` сам по себе не делает каталоги read-only. Сохранить approval, ограничить подключённые папки и защитить их правами/отдельной копией.

### Основания инвентаризации

- Профиль и identity: `/home/max/.hermes/profiles/bb-hunter/config.yaml`, `/home/max/.hermes/profiles/bb-hunter/SOUL.md`.
- Скиллы: `/home/max/bb/graph-kb-assistant/.claude/skills/**/SKILL.md`, `/home/max/bb/.claude/skills/**/SKILL.md`.
- MCP catalog/source of truth в локальном checkout: `internal/mcpserver/server.go` (tool schemas/read-only annotations), `internal/mcpserver/consultation.go` (consultation write annotations), `tests/test_hermes_setup.py` (15 expected tools, без raw tool), `docs/SETUP.md` и `docs/CURRENT-CAPABILITIES.md`.
- Launcher: `/home/max/.local/bin/bb-assistant-mcp-pve` (SSH wrapper; не запускался).
- Локальная документация Hermes: `/home/max/.hermes/hermes-agent/website/docs/user-guide/features/delegation.md`, `.../developer-guide/delegation-provider-plugin.md`, `.../user-guide/configuration.md`, `.../user-guide/features/skills.md`.
