---
name: subfinder playbook
description: Subfinder passive subdomain enumeration reference covering source selection, recursion, rate limits, JSONL output, and pipeline hand-off.
---

# Subfinder Playbook

Reference for passive subdomain enumeration with Subfinder: source selection, recursion control, per-source rate limits, and pipeline-ready output. Pull this in when you need to recall flag interactions, troubleshoot low result counts, or chain subfinder output into httpx/naabu/nuclei.

Upstream: https://docs.projectdiscovery.io/opensource/subfinder/usage

## RedAmon wiring

| Action | Tool | Notes |
|---|---|---|
| Run passive enumeration | `execute_subfinder` | Pass arguments without the leading `subfinder`. |
| Active subdomain brute-force fallback | `kali_shell` | `dnsx -l roots.txt -d wordlist -resp -silent` or `amass enum -active -brute -d <domain>`. |
| Cert transparency cross-check | `kali_shell` | `curl -s 'https://crt.sh/?q=%25.<domain>&output=json'` |

## Canonical shape

```
subfinder (-d DOMAIN | -dL FILE) [sources] [throughput] [output]
```

Subfinder is **passive only**: it never sends traffic to the target. Active resolution requires `dnsx`, `amass -active`, or `httpx -nf` afterwards.

## Flag reference

### Targeting

| Flag | Purpose |
|---|---|
| `-d <domain>` | Single root |
| `-dL <file>` | List of roots (one per line) |

### Source control

| Flag | Purpose |
|---|---|
| `-all` | Enable all configured sources (paid + recursive) |
| `-recursive` | Use recursive-capable sources |
| `-s <list>` | Include specific sources (`crtsh,virustotal,dnsdumpster`) |
| `-es <list>` | Exclude sources |
| `-ls` | List configured sources |
| `-pc <file>` | Custom provider config (default `~/.config/subfinder/provider-config.yaml`) |

### Throughput

| Flag | Purpose |
|---|---|
| `-rl <n>` | Global rate limit (req/s, applies to source APIs) |
| `-rls 'src=n/s,...'` | Per-source rate limit |
| `-timeout <s>` | Source request timeout |
| `-max-time <m>` | Total enumeration cap, **minutes** |
| `-proxy <url>` | Proxy outbound source requests |

### Output / filtering

| Flag | Purpose |
|---|---|
| `-silent` | Compact output |
| `-o <file>` | Output file |
| `-oJ` `-json` | JSONL output |
| `-cs` `-collect-sources` | Include source metadata (requires `-oJ`) |
| `-nW` `-active` | Filter to active subdomains only (resolves DNS) |
| `-ip` | Append resolved IPs |

## Default safe invocation

```
execute_subfinder args: "-d example.com -all -recursive -rl 20 -timeout 30 -silent -oJ -o /tmp/subfinder.jsonl"
```

## Recipes

### Standard passive enum

```
execute_subfinder args: "-d example.com -silent -o /tmp/subs.txt"
```

### Broad-source enum across many roots

```
execute_subfinder args: "-dL /tmp/roots.txt -all -recursive -rl 20 -silent -oJ -o /tmp/subfinder_multi.jsonl"
```

### Source-attributed JSONL

```
execute_subfinder args: "-d example.com -all -oJ -cs -o /tmp/subfinder_sources.jsonl"
```

Output rows look like:

```
{"host":"api.example.com","sources":["crtsh","alienvault","virustotal"]}
```

Useful when you want to weight findings: a host attested by 3+ independent sources is almost certainly real.

### Active-only filter (resolves immediately)

```
execute_subfinder args: "-d example.com -all -recursive -nW -silent -o /tmp/subs_active.txt"
```

Note: `-nW` drops every passive hit that does not currently resolve. You will lose subdomains that exist behind name-server delays, internal-only zones, or temporary outages. Prefer keeping the full passive list and resolving with `dnsx` separately.

### Through a proxy (auditable provider traffic)

```
execute_subfinder args: "-d example.com -all -recursive -proxy http://127.0.0.1:48080 -silent -oJ -o /tmp/subfinder_proxy.jsonl"
```

### Provider config check

```
kali_shell: subfinder -ls
kali_shell: cat ~/.config/subfinder/provider-config.yaml
```

If a paid provider lacks an API key, subfinder silently skips it. Low result counts are usually a config issue, not a target issue.

## Tuning matrix

| Symptom | Adjustment |
|---|---|
| Very low results | Add `-all -recursive`, verify provider keys, drop `-nW` |
| Provider 429 / errors | Lower `-rl`, set `-rls 'crtsh=5/s,virustotal=2/s'` |
| Long runtime on many roots | Set `-max-time 5` and split the list, or run in parallel batches via `-dL` chunks |
| Output mixes wildcards | Filter downstream with `dnsx -l ... -filter-wildcard` |

## Active resolution chain

Subfinder is the first half of the recon move; the second half is active validation:

```
execute_subfinder args: "-d example.com -all -recursive -silent -o /tmp/subs.txt"
kali_shell: dnsx -l /tmp/subs.txt -a -resp -silent -o /tmp/subs_resolved.txt
kali_shell: cut -d ' ' -f1 /tmp/subs_resolved.txt | sort -u > /tmp/live_subs.txt
execute_httpx args: "-l /tmp/live_subs.txt -sc -title -td -silent -j -o /tmp/httpx.jsonl"
```

## Pitfalls

- `-cs` produces no source metadata without `-oJ`.
- `-nW` is destructive on passive-only flows; keep the unfiltered list.
- Subfinder reports unique hostnames per run, but `crt.sh` may include wildcards (`*.target.tld`); dedupe and strip wildcards before resolving.
- Provider config must include API keys for VirusTotal, SecurityTrails, BinaryEdge, Censys, Shodan, and similar; without keys those sources are silently dropped (subfinder will not error).
- Running on many roots concurrently is fine; running with very high `-rl` triggers rate limits at the providers (not the target).

## Hand-off

```
subfinder -d <domain> -> /tmp/subs.txt
   -> dnsx -l /tmp/subs.txt -a -resp -> /tmp/live_subs.txt
   -> execute_httpx -l /tmp/live_subs.txt -sc -title -td -j -> /tmp/httpx.jsonl
   -> execute_nuclei -l /tmp/live_subs.txt -as -j -> /tmp/nuclei.jsonl
   -> kali_shell subzy run --targets /tmp/live_subs.txt -> takeover candidates
```
