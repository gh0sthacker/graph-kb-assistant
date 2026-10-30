# RedAmon Recon Pipeline — backlog покрытия

> Срез по приложенному скриншоту и локальным поверхностям продукта. Проверены **41** skill в `/home/max/bb/.claude/skills`, **6** skill в `graph-kb-assistant/.claude/skills`, а также каталог MCP в `internal/mcpserver/server.go` (для consult-tools — добавляемый им каталог из `consultation.go`). Никакие сканеры и запросы к целям не запускались.
>
> **Сводка:** 48 техник всего — **16 covered**, **30 partial**, **2 missing**; из них 34 узла прочитаны со скриншота и 14 добавлены сверх скрина.

## Как читать матрицу

- `covered` — есть skill или инструмент с прямым, документированным назначением. Это не означает, что техника запускалась или что target coverage проверен.
- `partial` — есть смежный сбор/анализ или ручной процесс, но нет полного специализированного этапа, доказательного закрытия либо интеграции результата в граф.
- `missing` — в проверенных каталогах нет специализированного этапа/инструмента для этой техники.
- Приоритет относится к продуктовому покрытию: P0 — быстро добавляет проверяемые узлы/рёбра и evidence в graph+KB; P1 — важное специализированное покрытие; P2 — нишевое/дорогое или повышенно рискованное покрытие.
- `[скрин]` — читаемый узел на изображении; `[добавлено сверх скрина]` — самостоятельный пункт бэклога сверх изображения.

## Узлы и связи на скриншоте

### Читаемые узлы-техники (34)

Subdomain Discovery; URLScan; Uncover; Shodan; OSINT Enrichment; Naabu; Masscan; Nmap; TLS Grab; Httpx; Katana; ZAP Ajax Spider; Hakrawler; **jsluice** (на скрине; в переданном описании также встречается «Juce»); ffuf; GAU; ParamSpider; Kiterunner; Arjun; OpenAPI; Endpoint AI Classifier; JS Recon; Serialized Objects; AI Surface Recon; Supply Chain; Nuclei; GraphQL Scan; Subdomain Takeover; VHost & SNI; Cache Poisoning; CVE Lookup; MITRE; Security Checks; Origin Discovery.

### Читаемые категории/типы сущностей

- Внизу графа читаются: `Parameter (7)`, `Header (12)`, `Technology`, `DNSRecord`, `Certificate`, `Package`, `Secret`, `MisPackageFinding`, `ExternalDomain (1)`, `Vulnerability`, `MitreData`.
- Один ярлык выглядит как `Capes`; возможно, это `CAPEC`, но по изображению подтверждать такую нормализацию нельзя.
- В постановке задачи также названы `Passive DNS`, `Portscan`, `Nuclei`, `Vulnerable`, `Exploit` и `Certificates`. Считаю это дополнительными пользовательскими метками/группами, а не подтверждёнными OCR-подписями на скриншоте.

### Связи / поток данных

Скриншот показывает общий поток слева направо: разведка доменов и OSINT → порты/хосты → HTTP/TLS и технологический профиль → crawling, URL/API/JS и параметры → классификация поверхности/специализированные проверки → уязвимости, CVE/MITRE и итоговые проверки. Видно также, что инструменты соединены пунктирными линиями с категориями-артефактами внизу.

Надёжно восстановить **каждое отдельное ребро техника→техника** и его направление по этому растру нельзя: линии пересекаются, а стрелки/точки привязки слишком малы. Поэтому ниже перечислены связи на уровне этапов и ожидаемых артефактов, но они помечены как реконструкция потока, а не как экспорт точного edge list. Для точного списка рёбер нужен исходный граф/экспорт RedAmon.

Реконструкция основных зависимостей по расположению и подписанным типам данных:

1. `Subdomain Discovery`, `URLScan`, `Uncover`, `Shodan`, `OSINT Enrichment` → внешние домены/URL и DNS/Passive-DNS сигналы (`ExternalDomain`, `DNSRecord`).
2. Активы и IP из разведки → `Naabu` / `Masscan` / `Nmap` → порты и сервисы (`Portscan`, `Technology`). Это вход для `Httpx`, `TLS Grab` и скрытых host/SNI-проверок.
3. HTTP/TLS-профиль и архивные URL → `Katana`, `ZAP Ajax Spider`, `Hakrawler`, `GAU`, `ffuf` → URL/endpoint/параметры и заголовки (`Parameter`, `Header`).
4. JS/клиентские assets и API surface → `jsluice`, `JS Recon`, `OpenAPI`, `Arjun`, `ParamSpider`, `Kiterunner` → кандидаты endpoint/параметров, JS-сигналы/секреты и serialized-object сигналы.
5. Собранная поверхность → `Endpoint AI Classifier` / `AI Surface Recon` и проверки `Nuclei`, `GraphQL Scan`, `Subdomain Takeover`, `VHost & SNI`, `Cache Poisoning` → гипотезы/уязвимости и package/security-сигналы.
6. Технологии/версии и найденные гипотезы → `CVE Lookup` → `MITRE`/`MitreData` → `Security Checks`; DNS/cert/IP/vhost-сигналы также ведут к `Origin Discovery` и takeover-кандидатам.

## Матрица покрытия

| Техника | Категория | Что даёт | Статус у нас | Чем закрыть (скилл/инструмент) | Приоритет |
|---|---|---|---|---|---|
| [скрин] Subdomain Discovery | Passive DNS / DNSRecord / ExternalDomain | Кандидаты поддоменов с provenance источника | covered | `passive-recon`; для сохранённых артефактов — `bb_surface_map`, `bb_graph_slice` | P0 — базовые asset-узлы и связи |
| [скрин] URLScan | URL / ExternalDomain | Пассивные URL, связанные с доменом и страницами | partial | `passive-recon`, `host-recon` покрывают архивные URL; отдельного URLScan connector нет | P1 — обогащает endpoint-граф, но не блокирует основной сбор |
| [скрин] Uncover | Passive OSINT / ExternalDomain | Поиск хостов/сервисов в поддерживаемых источниках | partial | `passive-recon` даёт многоисточниковый OSINT, но `uncover` как отдельный источник не подтверждён | P1 — дополнительная полнота источников |
| [скрин] Shodan | OSINT / Portscan / Technology | Внешние сведения об IP, портах и сервисах | covered | `passive-recon` включает Shodan enrichment | P1 — полезен для привязки внешних сервисов к активам |
| [скрин] OSINT Enrichment | OSINT / ExternalDomain | ASN, IP, домены, утечки, публичные URL и внешние связи | covered | `passive-recon`; результаты можно представить в graph через локальные graph MCP | P0 — много новых связуемых assets |
| [скрин] Naabu | Portscan | Список портов/хостов для проверки HTTP и сервиса | covered | `passive-recon` включает passive portscan; `port-scan` — masscan→nmap активный этап | P0 — соединяет активы с наблюдаемыми сервисами |
| [скрин] Masscan | Portscan | Быстрое обнаружение открытых портов | covered | `port-scan` | P1 — полезен, но активен и требует разрешения |
| [скрин] Nmap | Portscan / Technology | Состояние порта, banner, сервис и версия | covered | `port-scan` | P1 — проверяемые сервисные факты для графа/CVE |
| [скрин] TLS Grab | Certificate / DNSRecord | Сертификат, SAN/CN, связи имён и хостов | covered | `host-recon`, `passive-recon` (TLS и SAN) | P0 — сертификаты дают сильные asset-рёбра |
| [скрин] Httpx | Technology / Header | HTTP reachability, redirect, заголовки и tech hints | covered | `host-recon`, `passive-recon`, `vhost-probe` | P0 — нормализует HTTP-узлы и fingerprints |
| [скрин] Katana | URL / Endpoint | Рекурсивно находит страницы и ссылки | partial | `host-recon` инвентаризирует страницы/routes; `katana` упомянута как зависимость, но crawler-процедура не является обязательным покрытым шагом | P1 — закрыть систематический crawl + coverage |
| [скрин] ZAP Ajax Spider | URL / Endpoint / JS | JS-rendered страницы и клиентские переходы | partial | `browser-app-map` покрывает read-only браузерные flows, но не ZAP spider и не полное JS crawling | P1 — динамические UI/API связи |
| [скрин] Hakrawler | URL / Endpoint | Ссылки/пути из HTML и страниц | partial | `host-recon`, `browser-app-map`; нет отдельного crawler-прохода Hakrawler | P1 — недостающие страницы/роуты |
| [скрин] jsluice | JS / Endpoint / Parameter | URL и endpoint-кандидаты из JavaScript | covered | `js-recon` использует `jsluice`, фиксирует call-site и coverage | P0 — source evidence и route-to-feature рёбра |
| [скрин] ffuf | Endpoint / Parameter | Активный content/parameter discovery | covered | `web-fuzz`, `mass-fuzz`; строгие scope/rate gates | P1 — расширяет поверхность, но требует явного разрешения |
| [скрин] GAU | URL / Endpoint | URL из публичных архивов | covered | `passive-recon`, `host-recon` (gau/архивные URL) | P1 — дешёвое расширение URL-графа |
| [скрин] ParamSpider | Parameter / URL | Кандидаты параметризованных archived URL | partial | `param-extractor` извлекает наблюдавшиеся параметры; `js-recon` — параметры из JS; нет ParamSpider workflow | P1 — исторические параметры помогают тест-плану |
| [скрин] Kiterunner | API / Endpoint | API route discovery по словарю и ответам | partial | `api-caido-investigation`, `js-recon`, `web-fuzz` закрывают известные endpoint/URL; нет специализированного API wordlist scanner | P1 — API route coverage остаётся неполной |
| [скрин] Arjun | Parameter | Имена скрытых параметров через активные проверки | partial | `param-extractor` извлекает наблюдавшиеся параметры; `web-fuzz` — активный общий fuzzing, но не Arjun-style discovery | P1 — закрывает параметры вне трафика/JS |
| [скрин] OpenAPI | API schema / Endpoint | API operations, методы, поля и схемы | partial | `js-recon`, `application-map`, `api-caido-investigation` моделируют endpoint/поля; нет отдельного OpenAPI ingest+schema parser | P0 — схема даёт проверяемые API-узлы и ожидаемые поля |
| [скрин] Endpoint AI Classifier | Endpoint / Technology | Категоризация и приоритизация endpoint-кандидатов | partial | `js-endpoint-mapper` (правила/эвристики) и `traffic-triage` (категории ASVS); не общий AI classifier | P0 — нужен единый уверенный типизированный endpoint-граф |
| [скрин] JS Recon | JS / Endpoint / Secret | Assets, endpoints, transports, call-sites и coverage | covered | `js-recon`; `application-map` соединяет JS с UI и Caido | P0 — один из самых плотных источников графовых связей |
| [скрин] Serialized Objects | Serialized Objects / Vulnerability | Находит форматы сериализации и небезопасные десериализаторы | partial | `traffic-triage` выявляет deserialization-кандидаты; `web-investigation` охватывает parser behavior, но нет специализированного discovery | P2 — важная, но более узкая поверхность |
| [скрин] AI Surface Recon | Endpoint / Vulnerability | AI-сводка attack surface и кандидатов из evidence | partial | `graph-guided-vulnerability-analysis`, `traffic-triage`, `bb_graph_slice` поддерживают анализ уже собранных данных; нет recon-агента, который сам строит поверхность | P0 — усилить интерпретацию графа без потери provenance |
| [скрин] Supply Chain | Package / MisPackageFinding | Зависимости, package risks и supply-chain findings | missing | Специализированный SBOM/OSV/provenance scanner отсутствует; `cve-matcher` прямо не покрывает audit package manifests | P1 — важные уязвимости пакетов не видны из host fingerprint |
| [скрин] Nuclei | Nuclei / Vulnerability | Шаблонные проверки известной поверхности | partial | `cve-matcher` допускает opt-in nuclei verification, `cms-check` содержит gated Nuclei; нет общего nuclei pipeline с graph/evidence output | P1 — высокий выигрыш при строгом allowlist и воспроизводимом evidence |
| [скрин] GraphQL Scan | GraphQL / Endpoint / Vulnerability | Операции, schema exposure, auth/complexity кандидаты | partial | `js-recon`, `traffic-triage`, `api-caido-investigation`; нет отдельного GraphQL scanner и graph schema ingestion | P1 — GraphQL часто несёт много функций в одной точке |
| [скрин] Subdomain Takeover | DNSRecord / Vulnerability | Dangling DNS и возможность захвата внешнего ресурса | partial | `passive-recon` даёт домены/DNS, `hist-dns` — stale IP; нет полного CNAME-provider fingerprint + ownership proof | P0 — потенциально высокое влияние, DNS evidence хорошо моделируется |
| [скрин] VHost & SNI | Certificate / ExternalDomain / DNSRecord | Скрытый virtual host через Host/SNI/cert | covered | `vhost-probe` (Host-header fuzz, TLS CN/SAN, httpx verification) | P1 — прямое дополнение asset/host графа |
| [скрин] Cache Poisoning | Header / Vulnerability | Несоответствие cache key и ответа на варианты запроса | partial | `response-anomaly` и `traffic-triage` отмечают response/cache сигналы; нет контролируемой cache-key/poison validation workflow | P2 — требует точных повторов и контроля кэша |
| [скрин] CVE Lookup | Vulnerability / CVE | Связь tech/version с CVE, KEV, EPSS и PoC | covered | `cve-matcher`, `bb_kb_search`, `bb_kb_get_source` | P0 — сразу связывает fingerprint с KB/evidence, не доказывая exploitability |
| [скрин] MITRE | MITRE / MitreData / CAPEC? | Таксономия техник/уязвимостей для нормализации и отчёта | partial | `traffic-triage` маппит категории к OWASP ASVS; KB содержит MITRE ATT&CK, но нет общего output mapper к ATT&CK/CAPEC | P1 — улучшает поиск и дедупликацию гипотез |
| [скрин] Security Checks | Vulnerability / Exploit | Набор проверок по выявленной поверхности | partial | `web-investigation`, `traffic-triage`, `auth-boundary-assist`; проверки разнесены по gated workflows, нет общего набора/счётчика покрытия | P0 — важно связать каждый check с target evidence и coverage gap |
| [скрин] Origin Discovery | DNSRecord / ExternalDomain / Vulnerability | Поиск origin за CDN/WAF по историческим IP/сопоставлению хоста | partial | `hist-dns` ищет stale IP, `vhost-probe` сопоставляет имя/IP; нет широкого origin discovery и графового confidence model | P1 — high impact, но часто неполные/шумные сигналы |
| [добавлено сверх скрина] JS bundle secret detection + source maps | Secret / JS / Package | Находит вероятные ключи/токены с файлом, диапазоном и hash; значение хранится редактированным | partial | `js-recon` допускает отдельный secret-detection sidecar; `secret-chain` проверяет найденный секрет только по явному запросу; нет единого evidence/coverage schema для bundles | P0 — секреты дают проверяемые связи asset→secret→service, важна redaction |
| [добавлено сверх скрина] OpenAPI/Swagger schema ingestion и нормализация | API schema / Endpoint / Parameter | Импортирует operations, параметры, схемы request/response и version | partial | `application-map`, `js-recon`, `api-caido-investigation`; построить parser/normalizer с source hash и ссылками на endpoint nodes | P0 — схема повышает полноту и валидирует endpoint discovery |
| [добавлено сверх скрина] WebSocket/SSE endpoint mapping и checks | WebSocket / Endpoint / Header | Транспорт, handshake, channel/topic, auth и сообщения | partial | `js-recon` ищет WebSocket/SSE hints, `traffic-triage` категоризует websocket; browser helper явно блокирует WS, поэтому runtime coverage отсутствует | P1 — иначе карта ложно выглядит полной для real-time приложений |
| [добавлено сверх скрина] gRPC/protobuf/WSDL schema inventory | API schema / Endpoint | RPC methods, message fields и transport links | partial | `js-recon`/`application-map` сохраняют RPC/gRPC hints, но parser/schema capture нет | P2 — закрывает не-REST поверхности в целевых приложениях |
| [добавлено сверх скрина] JWT/JOSE structural и policy analysis | Secret / Header / Vulnerability | Алгоритм, issuer/audience/expiry, key binding, revocation и flow context | covered | `auth-boundary-assist` содержит JWT/JOSE review matrix; не выполняет forgery и не отправляет запросы автоматически | P1 — высокая ценность для auth graph и проверяемого анализа |
| [добавлено сверх скрина] SSRF sink mapping и безопасная проверка | Parameter / Endpoint / Vulnerability | URL/file/webhook sinks, источник параметра и bounded evidence для SSRF-кандидата | partial | `traffic-triage`, `param-extractor`, `web-investigation`; автоматические external callbacks отдельно gated, единого source→sink graph этапа нет | P0 — связывает входные параметры, endpoint и проверяемое impact evidence |
| [добавлено сверх скрина] Dangling CNAME/DNS takeover proof | DNSRecord / Vulnerability | CNAME chain, provider fingerprint, unresolved target и доказательство контроля/занятости | partial | `passive-recon`, `hist-dns`; отдельный CNAME provider validator и immutable proof отсутствуют | P0 — конкретизирует screenshot Subdomain Takeover и снижает false positives |
| [добавлено сверх скрина] CORS policy validation | Header / Vulnerability | Origin reflection, credentials, preflight и cache variation сигналы | partial | `traffic-triage` помечает CORS misconfiguration; `web-investigation` может валидировать bounded hypotheses, но нет выделенного полного validator | P1 — проверяемая политика, легко связать с endpoint/auth контекстом |
| [добавлено сверх скрина] OAuth/OIDC flow и PKCE validation | Endpoint / Header / Vulnerability | redirect/state/nonce/PKCE, issuer/client и token-flow relationships | partial | `traffic-triage` классифицирует oauth_oidc, `auth-boundary-assist` покрывает auth boundaries; нет полного протокольного inventory/checker | P1 — даёт auth-flow граф и снижает риск пропустить account-boundary flaws |
| [добавлено сверх скрина] BOLA/IDOR actor-object graph matrix | Endpoint / Parameter / Vulnerability | Связь actor/session→object→request→response и контраст доступа | covered | `api-caido-investigation`, `auth-boundary-assist`, `web-investigation`; локальные `bb_graph_slice`/`bb_get_capture` дают evidence refs | P0 — максимально соответствует graph+evidence модели |
| [добавлено сверх скрина] HTTP request smuggling / desync | Header / Vulnerability | Различия парсинга proxy/backend и доказательство desync | missing | В проверенных skills и MCP catalog отдельного safe desync workflow/tool нет | P2 — нишевая, рискованная техника; только изолированные, строго авторизованные проверки |
| [добавлено сверх скрина] TLS posture beyond certificate grab | Certificate / Technology / Header | Версии TLS, cipher suites, hostname/chain, weak policy и HSTS | partial | `host-recon`/`port-scan` фиксируют TLS/cert сведения; отдельного полного posture auditor нет | P1 — обогащает asset/service baseline, часть проверок детерминируема |
| [добавлено сверх скрина] Third-party JS / CSP / SRI trust graph | Package / ExternalDomain / Header / JS | Связывает script origin, host, CSP/SRI и dependency trust boundaries | partial | `application-map` записывает third-party dependencies, `js-recon` учитывает external assets; CSP/SRI/integrity graph отсутствует | P1 — supply-chain путь через браузерный runtime |
| [добавлено сверх скрина] API schema-to-runtime drift | API schema / Endpoint / Evidence | Сравнивает заявленные operations/fields со source-only и Caido-observed поведением | partial | `application-map`, `api-caido-investigation`, `bb_search_captures`, `bb_graph_slice`; отдельного diff/coverage artifact нет | P0 — прямое преобразование разрозненных фактов в проверяемые графовые gaps |

## Что у нас нет и почему это важно

1. **Supply-chain/SBOM scanner (`missing`).** `cve-matcher` работает по fingerprint компонентов, но не по dependency manifest/SBOM; локальный MCP также не запускает аудит. Без этого остаются незамеченными уязвимости библиотек, транзитивные зависимости и package provenance, которые не видны по HTTP-заголовку.
2. **HTTP request smuggling/desync (`missing`).** Нет специализированного протокольного workflow или инструмента, который сравнивает фронтенд/бэкенд parser behavior. Это отдельный высокорисковый класс, не покрываемый generic web-fuzz или response outlier ranking.
3. **Недостающая интеграция crawler/API schema → graph (`partial`).** `host-recon`, `js-recon` и `application-map` дают богатые данные, однако нет единой записи, которая сводит URL, operation/schema, JS call-site, observed Caido exchange, actor/object и coverage gap с provenance.
4. **Отсутствие специализированной записи secret evidence (`partial`).** `js-recon` допускает отдельный поиск секретов, а `secret-chain` — отдельную проверку найденного секрета. Не хватает безопасной нормализации находки с точным source locator/hash, redacted value, типом и связями с bundle/доменом/пакетом.
5. **DNS takeover — не равно перечислению поддоменов (`partial`).** Нужны chain CNAME→provider, dangling-state и evidence, почему claim технически возможен. Простые stale DNS/IP и имя из CT/OSINT — только lead.
6. **WebSocket runtime coverage неполна (`partial`).** JS может указать на WebSocket/SSE, но browser helper блокирует WebSocket. Без отдельного разрешённого capture/test шага граф не должен считать этот транспорт закрытым.
7. **MCP — про локальное evidence, не про recon scanner.** `server.go` предоставляет `bb_list_projects`, `bb_search_captures`, `bb_surface_map`, `bb_get_capture`, `bb_graph_slice`, `bb_graph_neighborhood`, `bb_kb_stats`, `bb_kb_search`, `bb_kb_get_source`; `bb_get_capture_raw` условно скрыт настройкой. Каталог дополняется `bb_consult_create_session`, `bb_consult_compare`, `bb_consult_record_proposal`, `bb_consult_validate_proposal`, `bb_candidate_save`, `bb_consult_resume`. Эти инструменты читают/сопоставляют локальные Caido/graph/KB данные или сохраняют локальные консультационные артефакты; они не отправляют запросы к целям и не заменяют отсутствующие scanner integrations.

## Топ-10 на ближайшие шаги

1. **Задать общий evidence contract для recon-узлов** — source, timestamp, scope, confidence, coverage status, hash и точный locator; не смешивать observation с AI interpretation.
2. **Загрузить passive assets в graph** — домен→DNS/IP→ASN/ExternalDomain→certificate с источником и временем; начать с `passive-recon`/`hist-dns` артефактов.
3. **Сшить port/service и HTTP/TLS facts** — порт→service/version→HTTP origin→headers/cert/technology, включая явные unknown/partial состояния.
4. **Нормализовать URLs/endpoints/parameters** из crawler/GAU/ffuf, `jsluice`, browser flows и Caido; сохранять source call-site и request ID без route-only дедупликации.
5. **Добавить OpenAPI/Swagger parser и schema-to-runtime diff** — связывать declared operation, fields и observed exchange; каждое расхождение должно быть evidence-backed.
6. **Сделать JS bundle secret sidecar** — inventory bundles/source maps, точные locators и hash, хранить только тип/редактированное значение; никакого автоматического token validation.
7. **Довести endpoint classification до графового контракта** — объединить правила `js-endpoint-mapper` и `traffic-triage`, confidence и evidence kind; AI только предлагает labels.
8. **Построить actor→session→object auth matrix** — переиспользовать `auth-boundary-assist`/`api-caido-investigation`, привязать пары захвата и результаты к `bb_graph_slice`; не выводить vulnerability из одного edge.
9. **Добавить отдельный DNS takeover proof workflow** — CNAME chain/provider state, DNS snapshot и evidence-пакет; оставить активную claim-проверку отдельной явно разрешаемой операцией.
10. **Ввести gated validation adapters для SSRF, GraphQL, CORS, Nuclei и TLS checks** — каждое выполнение привязано к scope/rate/stop conditions; результат — отдельная проверяемая карточка, не автоматический finding.

## Приоритеты в целом

- **P0:** полнота и сшивка asset/endpoint/API graph, JS/source evidence, authz relations и проверяемые gaps. Это максимально использует уже собираемые данные и возможности локального MCP.
- **P1:** расширение crawler/API/DNS/CVE/security coverage и интеграции результатов с graph+KB; активные проверки остаются opt-in и scope-gated.
- **P2:** специализированные/узкие проверки (serialized objects, cache poisoning, request smuggling, отдельные RPC transports) после того, как базовый evidence contract и coverage accounting готовы.

## Источники локального покрытия

- Skills: `/home/max/bb/.claude/skills/*/SKILL.md` (41 файл) и `/home/max/bb/graph-kb-assistant/.claude/skills/*/SKILL.md` (6 файлов). В частности: `passive-recon`, `host-recon`, `hist-dns`, `port-scan`, `vhost-probe`, `js-recon`, `application-map`, `js-endpoint-mapper`, `param-extractor`, `web-fuzz`, `mass-fuzz`, `api-caido-investigation`, `cve-matcher`, `traffic-triage`, `response-anomaly`, `auth-boundary-assist`, `web-investigation`, `browser-app-map`, `secret-chain`, `graph-guided-vulnerability-analysis`, `evidence-triage-lifecycle`, `caido-history-graph`, `mcp-readonly-tool-design`.
- MCP: `graph-kb-assistant/internal/mcpserver/server.go` и `consultation.go` — опубликованный каталог инструментов и его annotations/schemas.
