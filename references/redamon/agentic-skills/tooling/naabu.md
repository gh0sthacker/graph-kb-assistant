---
name: naabu playbook
description: Naabu port-scanning reference covering connect vs SYN scan modes, rate controls, verification, and proxy support.
---

# Naabu Playbook

Reference for fast TCP port discovery with naabu: scan-type selection, rate/worker tuning, host-discovery toggling, and verified output. Pull this in when you need to recall flag interactions, decide between connect and SYN modes, or hand discovered ports off to nmap for service enrichment.

Upstream: https://docs.projectdiscovery.io/opensource/naabu/usage

## RedAmon wiring

| Action | Tool | Notes |
|---|---|---|
| Run port scans | `execute_naabu` | Pass arguments without the leading `naabu`. |
| Service enrichment on open ports | `execute_nmap` | Always pair: discover with naabu, enrich with `nmap -sV -sC -p <ports>`. |
| Parse JSONL | `kali_shell` | `jq -r '"\(.host):\(.port)"' /tmp/naabu.jsonl | sort -u`. |

## Canonical shape

```
naabu (-host h | -list f) (-p ports | -top-ports n) [scan-type] [rate] [output]
```

The sandbox usually lacks raw socket privilege, so default to `-scan-type c` (connect). Switch to `syn` only when you confirm root and it is genuinely needed for stealth or speed.

## Flag reference

### Targeting

| Flag | Purpose |
|---|---|
| `-host <h>` | Single host |
| `-list <f>` `-l <f>` | Hosts list (CIDR allowed) |
| `-p <list|range>` | Explicit ports (`80,443,8000-8100`) |
| `-top-ports <n|full>` | Top-N profile (`100`, `1000`, `full`) |
| `-exclude-ports <list>` | Skip noisy ports (`9100,53`) |
| `-exclude-hosts <list>` | Skip target subset |

### Scan behavior

| Flag | Purpose |
|---|---|
| `-scan-type c` | TCP connect (no privilege required) |
| `-scan-type s` `-scan-type syn` | SYN scan (root) |
| `-Pn` | Skip host discovery |
| `-rate <n>` | Packets per second |
| `-c <n>` | Worker count |
| `-timeout <ms>` | Per-probe timeout, **milliseconds** |
| `-retries <n>` | Retries per probe |
| `-verify` | Re-confirm open ports before reporting |
| `-source-ip <ip>` | Bind to specific source IP |
| `-proxy <socks5://...>` | SOCKS5 proxy (connect-mode only) |

### Output

| Flag | Purpose |
|---|---|
| `-silent` | Compact output |
| `-j` `-json` | JSONL output |
| `-o <file>` | Output file |
| `-stats` | Progress |

## Default safe invocation

```
execute_naabu args: "-list /tmp/hosts.txt -top-ports 100 -scan-type c -Pn -rate 300 -c 25 -timeout 1000 -retries 1 -verify -silent -j -o /tmp/naabu.jsonl"
```

## Recipes

### Top-100 ports across a host list

```
execute_naabu args: "-list /tmp/hosts.txt -top-ports 100 -scan-type c -rate 300 -c 25 -timeout 1000 -retries 1 -verify -silent -o /tmp/naabu_top100.txt"
```

### Web-only sweep

```
execute_naabu args: "-list /tmp/hosts.txt -p 80,443,8080,8443,8000,8888,9000 -scan-type c -rate 300 -c 25 -timeout 1000 -retries 1 -verify -silent"
```

### Single-host quick check

```
execute_naabu args: "-host target.tld -p 22,80,443,3306,3389,5432,6379,8080,8443,9200,27017 -scan-type c -rate 300 -c 25 -timeout 1000 -retries 1 -verify"
```

### Full-range sweep on a single host (slow, scoped only)

```
execute_naabu args: "-host target.tld -p - -scan-type c -rate 500 -c 50 -timeout 1500 -retries 1 -verify -silent -j -o /tmp/naabu_full.jsonl"
```

### Privileged SYN run (only with root + lab consent)

```
execute_naabu args: "-list /tmp/hosts.txt -top-ports 100 -scan-type syn -rate 500 -c 25 -timeout 1000 -retries 1 -verify -silent"
```

## Tuning matrix

Reach for adjustments in this order when scans drop or hammer the target:

| Symptom | Adjustment |
|---|---|
| Many missed ports | Lower `-rate` (300 -> 150), raise `-timeout` (1000 -> 1500), `-retries 2`, ensure `-verify` |
| Network unstable | `-c 10`, `-rate 100`, `-timeout 2000`, `-retries 3` |
| Host appears down | Drop `-Pn` or add it (try the opposite of current state) |
| Privilege error on SYN | `-scan-type c` |
| Output overlaps with naabu defaults | Always set `-silent` + `-j -o <file>` for reproducible runs |

## Pitfalls

- `-timeout` is in milliseconds, not seconds. `-timeout 10` will time out almost everything.
- Connect scan exhausts FDs at high `-c` -> keep `-c <= 50` in the sandbox.
- Cloud / WAF rate-limits trigger silent drops -> lower `-rate`, retry with `-retries 2`.
- `-p -` rapidly inflates runtime; only after explicit scope confirmation.
- SOCKS5 proxy + SYN does not work; proxies require connect-mode.

## Hand-off

```
kali_shell: jq -r '"\(.host):\(.port)"' /tmp/naabu.jsonl | sort -u > /tmp/open_endpoints.txt
kali_shell: jq -r 'select(.port==443 or .port==80 or .port==8080 or .port==8443) | "\(.host):\(.port)"' /tmp/naabu.jsonl > /tmp/web_endpoints.txt
```

Pipe results into:
- `execute_nmap -n -Pn -sV -sC -p <ports>` for service detection on each open set.
- `execute_httpx -l /tmp/web_endpoints.txt -sc -title -td -j` to fingerprint web stacks.
