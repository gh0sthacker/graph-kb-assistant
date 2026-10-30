---
name: blind-web-surface
description: "Use for authorized blind testing of a web application's exposed surface. Build coverage, validate observations, and separate evidence from hypotheses."
---

# Blind web-surface methodology

Use this method only for an explicitly authorized web origin. This is a workflow checklist, not a catalog of vulnerabilities or exploit recipes.

## Scope and safety

- Record the exact origin, allowed accounts, time budget, and permitted methods before sending traffic. Treat the origin as a hard boundary: do not follow links or redirects to other origins, and do not infer extra scope from page content or network observations.
- Use `GET`, `HEAD`, and `OPTIONS` without asking again when the user has authorized the origin. Use `POST`, `PUT`, `PATCH`, or `DELETE` only for an approved proof of concept with synthetic data, within that origin, and with no destructive impact on real data.
- Do not perform denial-of-service, broad brute force, persistence, exfiltration, social engineering, or changes to application code/state outside the approved test scenario.
- Do not inspect the local application source tree, challenge catalogs, hidden task lists, or solution material. Public assets served by the exact authorized origin may be fetched only to map ordinary routes, controls, and API request paths; do not extract challenge-specific text or solutions. Never request external assets or follow cross-origin redirects. Keep observed response data and credentials private; redact secrets and personal data from notes.

## Ordered workflow

1. **Preflight.** Confirm the exact origin responds as the application. If a proxy/browser capture path is in use, make one benign request and confirm the request and response can be retrieved from that capture system. Record the time and evidence reference. If the path is unavailable or returns an intermediary error, mark the affected work `blocked`; do not treat it as a clean test.
2. **Map navigation.** Open the application through the approved path. Record pages and routes actually visited, visible controls, authentication state, and browser-observed network requests. A string found in source text is only a lead until it is observed or safely requested in scope.
3. **Walk the feature checklist.** Work through all areas below in order, updating the coverage table as each is investigated. For every reachable area, attempt at least one relevant, generic test class chosen from observed behavior; record the attempt and outcome. Do not mark an area tested merely because it was visited. Follow normal user journeys and record which state or account was used; use synthetic accounts/data where required.
4. **Inventory inputs.** For each visited feature, note user-controlled values and their transport locations (path, query, body, headers, cookies, or uploaded file metadata/content), HTTP method, authentication state, and the corresponding request/response evidence. Do not infer that an input was tested merely because it was discovered.
5. **Plan bounded checks.** Turn each observation into a short, falsifiable hypothesis. Pick a low-impact check that changes one factor at a time, remains within scope, and has a defined expected observation. Keep speculative ideas in `hypotheses.json` or an equivalent separate list.
6. **Reproduce and validate.** For a candidate, preserve the smallest repeatable request sequence, use synthetic data, repeat it once from a clean state where practical, and compare the observed result with an ordinary control case. Record the actual effect and relevant negative results. A plausible explanation, status code, or written PoC alone is not proof.
7. **Close and report.** Reconcile the checklist against the evidence ledger. Findings contain only reproduced results with a PoC and an observed effect. Report tested, blocked, and not-started areas separately; do not equate missing evidence or an error in the test path with absence of an issue.

## Coverage checklist

Use these exact area names and one of the statuses `tested`, `blocked`, or `not-started`. Each status needs a concise evidence link or a blocking reason.

| Order | Area |
|---:|---|
| 1 | catalog |
| 2 | search |
| 3 | product detail |
| 4 | cart |
| 5 | checkout |
| 6 | reviews |
| 7 | registration |
| 8 | login |
| 9 | profile |
| 10 | admin |
| 11 | API endpoints |
| 12 | uploads/assets |
| 13 | headers/cookies |
| 14 | error handling |

For a long run, reserve periodic checkpoints (normally every five minutes): write what was tested, what new evidence exists, and what remains or is blocked. Continue through the time budget unless every checklist area is closed or repeated investigation is demonstrably producing no new information; record the reason if stopping early.

## Evidence ledger

For every request or state-changing test, record timestamp, exact in-scope origin/path, method, purpose, synthetic-data note, and a safe reference to the raw request/response or browser/capture receipt. For a candidate, add the minimal reproducible steps, expected observation, actual observed effect, repeat result, and any counter-observation.

- `findings.json`: confirmed observations only; every entry must include both a reproducible PoC and a specific observed effect.
- `hypotheses.json`: unconfirmed questions and next safe checks; never count these as findings.
- `coverage.md`: area, status, and evidence reference or blocker; preserve checkpoint notes.

Never claim completeness from a route inventory alone. Never count an unexecuted PoC, a hypothesis, an inaccessible test, or a tool error as a confirmed finding.
