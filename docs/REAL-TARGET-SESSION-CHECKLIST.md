# Real-target session checklist

This checklist prepares a bounded, evidence-backed first run through Graph
Assistant. It does not authorize target traffic by itself. Before target access,
use the selected program's current `AUTHORIZATION.md` and `scope.md` as the
source of truth.

## Before the session

- [ ] Select one authorized program and read its current authorization and
      scope files.
- [ ] Record exact origins/hosts, permitted methods, rate/concurrency and
      request budget, test-account rules, allowed time window, and stop
      conditions in the restricted run receipt.
- [ ] Confirm the pinned Caido project matches the selected program and scope.
      The current `caido-tail` intake has no exact-origin filter, so use a
      project dedicated to the authorized target; if it contains unrelated
      origins, do not import it.
- [ ] Confirm the browser is explicitly routed through that project's Caido
      proxy; if this cannot be verified, do not browse the target.
- [ ] Confirm the Graph Assistant service, graph backend, encrypted evidence
      key, and local storage are healthy. Keep secrets out of shared artifacts.
- [ ] Start anonymous and use only visible, read-only UI flows on exact
      in-scope origins. Respect the authorization's budget for page-load traffic.

## First application workflow

1. Capture one small authorized UI flow in Caido and correlate the visible
   steps with exact request IDs.
2. Run one bounded live-intake cycle for the dedicated project:
   `scripts/bb-assistant caido-tail --project-key <registered-project-key> --max-pages 1`.
   It uses a recent time window, one page of up to 50 request rows, a 90-second
   overlap, and a 256 KiB per-side body limit by default. It reads Caido history
   and writes the project's local capture/graph inputs; it sends no requests to
   the target. Review imported counts, parse/fidelity labels, graph provenance,
   pending projection jobs, and remaining coverage before enabling repeated
   `--watch` cycles.
3. Select an observed operation in the graph. Follow bounded pages and inspect
   the exact request/response evidence locally, including truncation and source
   integrity state.
4. Compare two observed captures or revisions and record the concrete
   request-side and response-side differences. Keep IDs and hashes in the
   restricted run evidence.
5. Build a redacted Hermes packet only when consultation is useful. Inspect
   every excerpt in Graph Assistant and accept that exact saved packet in the
   UI before asking Hermes (`openai-codex` / `gpt-6-luna`) to resume it. Ordinary
   graph navigation does not send evidence to the model.
6. Record facts, unknowns, contrasting explanations, and the smallest next
   check. Keep a candidate separate from a reproduced or verified finding.

## Stop conditions

Stop the run on any scope mismatch, out-of-scope origin, unexpected
state-changing action, rate-limit/service error, unverified proxy path,
unexpected account data, or resource threshold specified by the program. Do
not retry through direct networking or a different Caido project. Record the
reason and preserve the run as `PARTIAL` or `BLOCKED`.

## Coverage at handoff

Report browser, JavaScript, and application-map coverage separately. Keep the
result `PARTIAL` while source history, routes, actor context, response fidelity,
or required repeat checks remain unresolved. Never describe incomplete
coverage as “no findings.” Keep raw traffic, credentials, and tokens in the
restricted program evidence area; do not put them in the global KB or shared
status notes.

## Inputs needed when selecting tomorrow's target

Use the program files if they already exist. Otherwise provide the program
name or local authorization path, exact in-scope origin(s), allowed methods and
rate/budget, stop conditions, and the selected Caido project. Do not send
passwords or tokens in chat; use the approved login path and restricted local
runtime.
