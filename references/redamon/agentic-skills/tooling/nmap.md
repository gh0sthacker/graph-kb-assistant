---
name: nmap playbook
description: Two-pass nmap workflow reference covering host discovery, NSE enrichment, timing templates, and sandbox-friendly scoping.
---

# Nmap / NSE Playbook

Reference for scoped TCP/UDP scanning, NSE enrichment, and timing controls. Pull this in when you need to recall flag interactions, pick a `-T` template, or remember which NSE categories actually fire vulnerability checks.

Upstream: https://nmap.org/book/man.html

## RedAmon wiring

| Action | Tool | Notes |
|---|---|---|
| Run scoped nmap scans | `execute_nmap` | Pass arguments without the leading `nmap`. |
| Run broad port discovery first | `execute_naabu` | Faster than nmap for "what is open"; feed open ports back into nmap for `-sV -sC`. |
| Manual NSE category browsing | `kali_shell` | `ls /usr/share/nmap/scripts/ | grep -i <pattern>` |
| Parse XML output | `kali_shell` | `xmllint` or feed into `nmap-parse-output`. |

## Two-pass model

1. **Discovery pass** with `naabu` (or a tight `--top-ports 100` nmap run) -> list of open ports per host.
2. **Enrichment pass** with `nmap -sV -sC -p <comma_ports>` only on the open set.

Avoid wide single-shot `nmap -p- -A` runs in the sandbox. They are slow, noisy, and frequently truncated by the 5-minute `kali_shell` ceiling (`execute_nmap` handles longer runs but still benefits from scoping).

## Flag reference

### Discovery and resolution

| Flag | Purpose |
|---|---|
| `-Pn` | Skip ICMP / ping; assume host is up |
| `-n` | Skip DNS resolution (faster, less DNS noise) |
| `-PS <ports>` `-PA <ports>` | TCP SYN / ACK ping discovery |
| `-PU <ports>` | UDP discovery probe |
| `--source-port <n>` | Static source port (egress filter bypass) |
| `--data-length <n>` | Pad packets (rare evasion) |

### Scan types

| Flag | Purpose |
|---|---|
| `-sS` | SYN scan (root, default privileged) |
| `-sT` | TCP connect (no raw socket privilege required) |
| `-sU` | UDP scan |
| `-sV` | Service/version detection |
| `-sC` | Default NSE category |
| `-sN -sF -sX` | Null / FIN / Xmas (firewall probing) |
| `-O` | OS fingerprint |
| `-A` | `-sV -sC -O --traceroute` aggregate (heavy) |

### Port selection

| Flag | Purpose |
|---|---|
| `-p 22,80,443,8080,8443` | Explicit ports |
| `-p-` | All 65535 TCP ports (avoid in sandbox unless required) |
| `--top-ports <n>` | Top-N common ports |
| `-F` | Fast scan (top 100 ports) |
| `--exclude-ports <list>` | Skip noisy ports |

### Timing

| Flag | Purpose |
|---|---|
| `-T0..-T5` | `T0` paranoid, `T3` default, `T4` aggressive (good default in sandbox), `T5` insane |
| `--max-retries <n>` | Cap retransmissions per probe |
| `--host-timeout <t>` | Give up on slow hosts (always set in agent runs) |
| `--script-timeout <t>` | Bound NSE script runtime |
| `--max-rate <n>` `--min-rate <n>` | Packets per second floor/ceiling |

### NSE

| Flag | Purpose |
|---|---|
| `-sC` | Default category |
| `--script <selector>` | Categories or filenames |
| `--script-args 'k=v,k2=v2'` | Pass arguments to scripts |
| `--script-args-file <file>` | Argument file for long values |
| `--script-help <selector>` | Show script docs |

### Output

| Flag | Purpose |
|---|---|
| `-oN <file>` | Normal text |
| `-oX <file>` | XML |
| `-oG <file>` | Grepable |
| `-oA <prefix>` | All three above with one prefix |
| `--reason` | Explain how each port state was concluded |
| `-v` / `-vv` | Verbosity |

## Default safe baseline

```
execute_nmap args: "-n -Pn --open --top-ports 100 -T4 --max-retries 1 --host-timeout 90s -oA /tmp/nmap_quick <host>"
```

## Recipes

### Important-port-only fast pass

```
execute_nmap args: "-n -Pn -p 22,80,443,3306,3389,5432,6379,8080,8443,9200,27017 --open -T4 --max-retries 1 --host-timeout 90s <host>"
```

### Service + script enrichment (after naabu/quick pass)

```
execute_nmap args: "-n -Pn -sV -sC -p <comma_ports> --script-timeout 30s --host-timeout 3m -oA /tmp/nmap_services <host>"
```

### No-root fallback (TCP connect)

```
execute_nmap args: "-n -Pn -sT --top-ports 100 --open --host-timeout 90s <host>"
```

### UDP top-50 (slow; only when needed)

```
execute_nmap args: "-n -Pn -sU --top-ports 50 --open -T4 --max-retries 1 --host-timeout 5m <host>"
```

### Vuln-category NSE sweep

```
execute_nmap args: "-n -Pn -sV --script vuln,exploit -p <comma_ports> --script-timeout 60s --host-timeout 5m -oA /tmp/nmap_vuln <host>"
```

### SMB enumeration (no creds)

```
execute_nmap args: "-n -Pn -p 139,445 --script smb-os-discovery,smb-enum-shares,smb-enum-users,smb-vuln-* --script-timeout 60s -oA /tmp/nmap_smb <host>"
```

### TLS posture

```
execute_nmap args: "-n -Pn -p 443,8443 --script ssl-enum-ciphers,ssl-cert,ssl-known-key,ssl-heartbleed,ssl-poodle --script-timeout 60s <host>"
```

### HTTP service enumeration

```
execute_nmap args: "-n -Pn -p 80,443,8080,8443 --script http-title,http-server-header,http-headers,http-methods,http-enum --script-timeout 60s <host>"
```

## NSE categories worth knowing

| Category | Use |
|---|---|
| `default` (= `-sC`) | Safe baseline checks |
| `safe` | Non-intrusive |
| `discovery` | Identification and enumeration |
| `version` | Service version refinement |
| `vuln` | Known-CVE checks |
| `exploit` | Active exploitation (treat as Phase 2) |
| `intrusive` | Scripts that may crash services |
| `brute` | Credential brute-force |
| `auth` | Authentication checks (no brute) |
| `dos` | Service-impacting (almost never appropriate) |

Combine with logical operators: `--script "vuln and not exploit"`, `--script "smb-* and not brute"`.

## Pitfalls and recovery

- Host shows as down -> add `-Pn` (target may filter ICMP).
- Scan never finishes -> set `--host-timeout` and `--script-timeout`; tighten `-p` or `--top-ports`.
- NSE returns nothing on a known-vulnerable service -> rerun with `-sV --version-all` first; many scripts gate on a version match.
- UDP scans look "all open" -> UDP states are noisy; cross-check with `naabu -scan-type c` and re-issue with `-sV` for the few you trust.
- `connect` scan only sees a subset vs SYN -> expected; some hosts rate-limit completed handshakes more aggressively than half-opens.
- Output truncation in shell view -> use `-oA <prefix>` and `kali_shell cat /tmp/nmap_*.gnmap`.

## Hand-off

```
kali_shell: cat /tmp/nmap_services.gnmap | awk '/Ports:/{print $2,$0}'
kali_shell: xmllint --xpath '//port[state[@state="open"]]' /tmp/nmap_services.xml
```

Push open services into `query_graph` for downstream skill routing. Feed HTTP services into `execute_httpx`, then `execute_nuclei`.
