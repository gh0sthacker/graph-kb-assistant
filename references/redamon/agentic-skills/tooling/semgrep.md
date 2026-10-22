---
name: semgrep playbook
description: Semgrep CLI reference for source-aware static analysis with rule-pack selection, severity filtering, and a black-box bridge for operator-supplied repos.
---

# Semgrep Playbook

Reference for driving `semgrep` against source code. Pull this in when the operator hands you a repository URL, an uploaded archive, or a downloaded JS bundle and asks for static-analysis findings. RedAmon agents have no source access by default; the first half of this skill is the bridge that gets code into the sandbox, the second half is how to scan it.

Upstream: https://semgrep.dev/docs/cli-reference

## Black-box bridge: how to get source

Semgrep needs files on disk. The agent is otherwise black-box, so source must enter the sandbox through one of these explicit paths:

| Source provided by operator | How to ingest |
|---|---|
| Public Git URL (GitHub, GitLab, Gitea) | `kali_shell git clone --depth 1 <url> /tmp/src/<name>` |
| Public Git URL with submodules | `kali_shell git clone --depth 1 --recursive <url> /tmp/src/<name>` |
| Tarball / zip URL | `kali_shell curl -fsSL -o /tmp/src.tar.gz <url> && mkdir -p /tmp/src/<name> && tar -xzf /tmp/src.tar.gz -C /tmp/src/<name>` |
| Specific branch or tag | `kali_shell git clone --depth 1 --branch <ref> <url> /tmp/src/<name>` |
| Single JS bundle from a live site | `execute_curl url: "https://target.tld/_next/static/chunks/main.js"` then save to `/tmp/src/bundle/main.js` |
| `.map` source map | Pull the `.map` file, decompose with `kali_shell unsource <file>` (if installed) or write a Python helper via `execute_code` |

If the operator has not provided source, do not invent it. Ask, or pivot to live-site recon (`/skill nextjs`, `/skill graphql`, etc.).

## Tool wiring

| Action | Tool | Notes |
|---|---|---|
| Run semgrep | `kali_shell` | No dedicated wrapper. Always pass `--metrics=off` and `--quiet` in agent runs. |
| Inspect / filter findings | `kali_shell` | `jq` over the JSON output. |
| Dynamic confirmation of a finding | `execute_curl` / `execute_nuclei` | Black-box probe to prove the static finding is reachable. |

## Canonical shape

```
semgrep scan --config <ruleset> --metrics=off --json --output <file> [filters] <path>
```

Always set `--metrics=off`; semgrep phones home by default. Always pass an explicit `--config`. Always scope the path; do not run from `/`.

## Flag reference

### Targeting

| Flag | Purpose |
|---|---|
| `<path>` | Directory or file to scan (positional) |
| `--include 'glob'` | Only scan paths matching glob (e.g. `'**/*.py'`) |
| `--exclude 'glob'` | Skip paths matching glob (`'**/node_modules/**'`, `'**/.git/**'`) |
| `--baseline-commit <sha>` | Only report findings introduced after the baseline (delta scan) |

### Rules

| Flag | Purpose |
|---|---|
| `--config p/<pack>` | Registry rule pack |
| `--config /path/to/rule.yaml` | Local rule file or directory |
| `--config auto` | Auto-detect language and load default packs |
| `--exclude-rule <id>` | Suppress a noisy rule |
| `--severity ERROR\|WARNING\|INFO` | Filter by severity |

### Output

| Flag | Purpose |
|---|---|
| `--json` `--output <file>` | JSON, file output |
| `--sarif` `--output <file>` | SARIF for upload into other systems |
| `--quiet` | Suppress progress banner |
| `--error` | Non-zero exit when any finding is present |
| `--no-rewrite-rule-ids` | Keep upstream IDs verbatim |

### Throughput

| Flag | Purpose |
|---|---|
| `--jobs <n>` | Parallel workers (4 is a safe sandbox default) |
| `--timeout <s>` | Per-file timeout |
| `--timeout-threshold <n>` | Skip file after this many timeouts on it |
| `--max-memory <mb>` | Cap per-process RAM |
| `--max-target-bytes <n>` | Skip files larger than this |

### Engines

| Flag | Purpose |
|---|---|
| `--pro` | Pro engine (cross-file, taint). Requires login; falls back gracefully. |
| `--oss-only` | Force OSS engine (deterministic, no login) |

## Useful registry rule packs

| Pack | Use |
|---|---|
| `p/default` | Curated security-relevant rules across all languages |
| `p/owasp-top-ten` | OWASP-aligned subset |
| `p/security-audit` | Broader audit-grade pack |
| `p/secrets` | API keys, tokens, credentials in source |
| `p/python` | Python-specific rules (Django, Flask, FastAPI) |
| `p/javascript` | JS / TS / Node rules |
| `p/typescript` | TypeScript-specific patterns |
| `p/react` | React XSS, dangerouslySetInnerHTML, JSX issues |
| `p/nextjs` | Next.js-specific (RSC, Server Actions, middleware) |
| `p/golang` | Go rules |
| `p/java` | Java / Spring rules |
| `p/kotlin`, `p/swift` | Mobile rules |
| `p/django`, `p/flask` | Framework-specific |
| `p/dockerfile` | Container hardening |
| `p/terraform`, `p/cloudformation` | IaC |
| `p/sql-injection`, `p/xss`, `p/command-injection` | Single-class deep dives |

## Default safe invocation

```
kali_shell: semgrep scan --config p/default --metrics=off --json --output /tmp/semgrep.json --quiet --jobs 4 --timeout 20 /tmp/src/<name>
```

## Recipes

### High-severity-only (fast triage)

```
kali_shell: semgrep scan --config p/default --severity ERROR --metrics=off --json --output /tmp/semgrep_high.json --quiet /tmp/src/<name>
```

### Stack-aware sweep

```
kali_shell: semgrep scan --config p/default --config p/javascript --config p/typescript --config p/react --config p/nextjs --config p/secrets --metrics=off --json --output /tmp/semgrep_stack.json --quiet --jobs 4 /tmp/src/<name>
```

### Secrets-only fast pass

```
kali_shell: semgrep scan --config p/secrets --metrics=off --json --output /tmp/semgrep_secrets.json --quiet --jobs 4 /tmp/src/<name>
```

### OWASP-oriented SARIF for upstream tooling

```
kali_shell: semgrep scan --config p/owasp-top-ten --metrics=off --sarif --output /tmp/semgrep.sarif --quiet /tmp/src/<name>
```

### Scoped scan against a single service

```
kali_shell: semgrep scan --config p/default --metrics=off --json --output /tmp/semgrep_api.json --quiet /tmp/src/myrepo/services/api
```

### Pro engine with graceful fallback

```
kali_shell: semgrep scan --config p/default --pro --metrics=off --json --output /tmp/semgrep_pro.json --quiet /tmp/src/<name> || \
            semgrep scan --config p/default --oss-only --metrics=off --json --output /tmp/semgrep_pro.json --quiet /tmp/src/<name>
```

### Custom rule (single file)

Write the rule to disk via `execute_code`, then:

```
kali_shell: semgrep scan --config /tmp/my_rule.yaml --metrics=off --json --output /tmp/semgrep_custom.json --quiet /tmp/src/<name>
```

## Parsing findings with jq

```
kali_shell: jq '.results | length' /tmp/semgrep.json                                          # count
kali_shell: jq -r '.results[] | "\(.extra.severity)\t\(.check_id)\t\(.path):\(.start.line)"' /tmp/semgrep.json | sort -u
kali_shell: jq '.results[] | select(.extra.severity=="ERROR") | {id: .check_id, path, line: .start.line, msg: .extra.message}' /tmp/semgrep.json
kali_shell: jq -r '.results[] | select(.check_id|test("secret|api[_-]?key|token";"i")) | "\(.path):\(.start.line)\t\(.extra.lines)"' /tmp/semgrep.json
```

## Tuning ladder

| Symptom | Adjustment |
|---|---|
| Scan too slow | Narrow path, drop `--config` count, lower `--jobs`, raise `--timeout-threshold` |
| OOM / killed worker | `--max-memory 2048`, drop `--jobs` to 2 |
| Too many findings | Add `--severity ERROR`, drop noisy packs (`p/security-audit`), `--exclude-rule` per-id |
| Too few findings | Add `p/<lang>` packs, ensure rules apply to detected language, try `--pro` |
| Pro engine fails | Run `--oss-only` and document the loss of cross-file taint coverage |
| Source maps unparsed | Resolve `.map` files separately; `--include '**/*.map.js'` does nothing useful |

## Bridging static to dynamic

Findings from semgrep are not exploit proof. Always confirm dynamically:

```
1. Static finding: SQL string concatenation in services/api/users.py:42 -> /api/users?id=...
2. Black-box probe:  execute_curl url: "https://target.tld/api/users?id=1' AND SLEEP(5)-- "
3. If the response delays >= 5s, file as confirmed; otherwise downgrade to "code smell, not reachable" and explain why.
```

For a full exploitation pivot, hand off to `/skill sqlmap`, `/skill jwt_attacks`, or `/skill graphql` depending on the finding class.

## Common pitfalls

- Forgetting `--metrics=off` ships data to semgrep.dev. Set it every run.
- Running without `--config` triggers an interactive prompt; always pass an explicit pack.
- `--config auto` requires network egress to the semgrep registry; in air-gapped runs use a pre-fetched local rule directory.
- Findings on `node_modules/` or vendored dependencies are usually noise; `--exclude '**/node_modules/**'` and `--exclude '**/vendor/**'`.
- `--baseline-commit` requires a real git history; `--depth 1` clones don't have ancestor commits.
- Pro engine offline -> falls back to OSS but emits warnings; always check the `--output` file rather than stdout.

## Hand-off

Static findings rarely stand alone. Pair semgrep results with:

- Black-box probes against the same routes via `/skill nextjs` / `/skill fastapi` / `/skill nestjs`.
- Vulnerability classes via `/skill jwt_attacks`, `/skill csrf`, `/skill open_redirect`.
- Tool follow-ups: `betterleaks` for secrets, `trivy fs` (when installed) for dependency CVEs.
