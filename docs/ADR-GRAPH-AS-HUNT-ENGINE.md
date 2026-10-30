# ADR: граф как основной механизм поиска уязвимостей

Статус: принято к реализации 2026-10-30.
Область: `graph-kb-assistant` (Go, Neo4j, SQLite) + `hunter-tools` (Python).

## 1. Проблема

Сегодня граф хранит **поверхность**: операция → параметр (имя) → формы запроса и ответа.
Этого достаточно, чтобы перечислить «что есть в приложении», но недостаточно, чтобы ответить
на главный вопрос охоты: **что проверять и почему**.

Следствия:
- агент получает список эндпоинтов и имён параметров, но не знает, где параметр попадает в
  SQL, шаблон, файловую систему или серверный fetch;
- нет значений и типов параметров — нельзя отличить `id=1` от `q=' OR 1=1--`;
- нет измерений идентичности — BOLA/BFLA остаются угадыванием;
- нет цепей — «хеши → вход → смена пароля» не выражается в модели;
- в итоге граф не влияет на результат прогона, специалисты работают по `routes.json`.

## 2. Целевая модель

Четыре примитива (по онтологии Standoff: Information / Input-control / Identity / Execution)
и рёбра «что это открывает дальше».

### Узлы

| Узел | Смысл | Ключевые свойства |
|---|---|---|
| `Endpoint` | операция приложения | method, route_template, host, port, auth_required, state_changing |
| `Parameter` (Input-control) | входная точка | name, location (query/body/header/path/hash), observed_types[], samples[] (значения короче 32 символов, без секретов), distinct_count, reflected(bool), reflection_context (html/attr/js/json) |
| `Sink` (Execution) | приёмник данных | kind (sql/template/fs/shell/ssrf/deser/graphql/eval), engine (sqlite/postgres/js/node/…), evidence_kind (error_signature/stack_trace/timing/differential), confidence 0..1 |
| `Identity` | роль/сессия | label (anon/customer/admin/custom), token_ref (не значение), capabilities[] |
| `Info` | раскрытые данные | kind (password_hash/token/config/pii/file), where (endpoint), redacted=true |
| `Finding` | подтверждённая находка | class, evidence_refs[], verifier, fingerprint |

### Рёбра

| Ребро | Откуда → Куда | Что означает |
|---|---|---|
| `HAS_PARAM` | Endpoint → Parameter | параметр принимается операцией |
| `REACHES` | Parameter → Sink | данные параметра доходят до приёмника (с evidence) |
| `ACCESSES` | Identity → Endpoint | роль имеет доступ (status, auth_required) |
| `EXPOSES` | Endpoint → Info | операция отдаёт данные |
| `ENABLES` | Info → Identity | данные позволяют получить/усилить идентичность (например, хеш → вход) |
| `UNLOCKS` | Identity → Endpoint | полученная роль открывает операцию (было 401/403 → стало 200) |
| `CONFIRMS` | Finding → (Parameter\|Sink\|Endpoint) | находка подтверждает ребро |

Инвариант: **ребро без evidence не создаётся.** Любое `REACHES`, `EXPOSES`, `UNLOCKS`
обязано ссылаться на конкретный capture/наблюдение.

## 3. Извлечение (P1)

Стадия выполняется при импорте/проекции, детерминированно, без LLM.

1. **Параметры.** Из URL query, тела (JSON/form), заголовков, path-сегментов.
   Сохранять: имя, location, типы (string/int/bool/uuid), до 5 сэмплов длиной < 32 символов,
   число различных значений.
2. **Отражение.** Если значение параметра встречается в теле ответа — фиксировать
   `reflected=true` и контекст: HTML-текст, HTML-атрибут, JS-строка, JSON-значение.
3. **Sink-сигнатуры (по ответам).** Таблица правил, минимум:
   - `sql`: `SQLITE_ERROR`, `SQLSTATE`, `syntax error at or near`, `ORA-`,
     `You have an error in your SQL syntax`;
   - `template`: `TemplateSyntaxError`, `jinja2`, `Twig`, `freemarker`, `velocity`;
   - `eval/js`: `Function (<anonymous>)`, `ReferenceError`, `SyntaxError:`,
     `Unexpected identifier`;
   - `fs`: `ENOENT`, `EACCES`, `No such file or directory`, `Permission denied`;
   - `ssrf`: запрос, инициированный сервером (по наличию OOB-хита или по полю URL в запросе
     + сетевой ответ 200/302 без клиентского запроса);
   - `deser`: `pickle`, `unserialize`, `__proto__`, `ObjectInputStream`, `marshal`.
   Для каждого сигнала: kind, engine (если выводится), confidence 0.9 при точном стеке,
   0.6 при тексте ошибки без стека.
4. **Связывание.** Параметр → Sink, если запрос с этим параметром вернул сигнатуру,
   ЛИБО параметр отражается, ЛИБО есть дифференциальный эффект (разные ответы на
   истина/ложь, разные задержки > baseline×3).
5. **Идентичности.** Если в запросе был Authorization/cookie — фиксировать Identity
   (label из роли, если известна, иначе `custom#n`), ребро `ACCESSES` со статусом.
   Если без авторизации 401, а с ней 200 — ребро `UNLOCKS`.
6. **Info.** Регулярки на хеши (32/40/64 hex), JWT, приватные ключи, `password`
   в JSON — с обязательной редакцией значения.

## 4. Запрос «что проверять» (P2)

Единая ручка вместо сырых слайсов:

`bb_attack_surface(project_key, classes?[], limit=50)` → JSON:

```json
{
  "candidates": [
    {
      "id": "cand-1a2b",
      "class": "sqli",
      "endpoint": "GET /rest/products/search",
      "parameter": "q",
      "sink": {"kind": "sql", "engine": "sqlite", "confidence": 0.9},
      "why": ["error_signature:SQLITE_ERROR", "capture:1355", "reflected:false"],
      "auth": {"required": false},
      "novelty": "untested",
      "playbook": "playbooks/sqli.md",
      "suggested_check": "дифференциальные пробы q=... с контролем"
    }
  ],
  "uncovered": [{"class": "xxe", "reason": "нет XML input-point"}],
  "stats": {"endpoints": 144, "parameters": 612, "sinks": 7}
}
```

Ранжирование (детерминированное, без LLM):

```
score = sink_confidence
      × (0.5 + 0.5 × reflected_bonus)      # отражение усиливает XSS/инъекции
      × class_fit                           # параметр подходит классу по типу/имени
      × identity_bonus                      # есть две роли → доступны BOLA/BFLA
      × novelty(untested=1.0, tested=0.3)
      × state_changing_bonus                # для business-logic/BFLA
```

`class_fit` — таблица «тип/имя параметра → классы»: `id|uid|user` → idor/bfla; `q|search|filter`
→ sqli/nosql/xss; `url|redirect|next|return` → ssrf/open-redirect; `template|tpl` → ssti;
`file|path|name` → path-traversal; `price|qty|amount|coupon` → business-logic.

## 5. Цепи (P3)

Путь по рёбрам `EXPOSES → ENABLES → UNLOCKS → REACHES` даёт сценарий:
«`/rest/memories` отдаёт хеши → хеш даёт вход под пользователем → его роль открывает
`/api/Users` → параметр `role` доходит до mass assignment».
Такие пути отдаются отдельной секцией `chains[]` с длиной и evidence на каждом шаге.

## 6. Роль LLM

LLM **не решает**, есть ли путь и есть ли sink — это делает детерминированный слой.
LLM получает готовый подграф-кандидат и решает, как его проверить, оформляет PoC и отчёт.
Иначе мы меряем уверенность модели, а не достижимость.

## 7. Критерии приёмки (на живом проекте juice-shop-test)

1. `bb_attack_surface` возвращает ≥ 20 кандидатов, среди них обязательно:
   - `sqli` на `/rest/products/search`, параметр `q`, sink `sql`, evidence — сигнатура СУБД;
   - `eval/js` на `/rest/track-order/{id}` (в ответе `Function (<anonymous>)`);
   - `xss`-кандидат на параметре, который отражается в HTML;
   - `bopla/bfla` на `/api/Users` с параметром `role` при наличии двух идентичностей.
2. Каждый кандидат имеет `why[]` с ссылкой на конкретный capture — без исключений.
3. Триаж в свипе берёт кандидатов из `bb_attack_surface`, а не только из regex по путям.
4. В отчёте появится секция «покрытие по кандидатам»: сколько проверено/подтверждено/опровергнуто.
5. Ни один кандидат не строится без evidence-ссылки (тест на это обязателен).

## 8. Что это меняет в продукте

- граф перестаёт быть витриной и становится **источником плана охоты**;
- появляется измеримая метрика: доля кандидатов, доведённых до вердикта;
- специалисты получают не «эндпоинт», а «эндпоинт + параметр + приёмник + почему» —
  это ровно то, что у RedAmon видно глазами, а у нас станет машинным контрактом.
