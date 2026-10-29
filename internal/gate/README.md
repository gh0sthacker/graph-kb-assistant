# Data-readiness gate

Run before starting a hunt and again after network/recon stages. A readiness
verdict does **not** certify complete route coverage or a vulnerability-free target.
This command is standalone: callers must stop their pipeline on a nonzero exit.

```sh
# Set CAIDO_ACCESS_TOKEN in the environment (never include it in reports).
bb-assistant gate --target https://authorized.example \
  --caido-url http://127.0.0.1:8080 --recon-dir ./recon --json
```

- `valid` (exit **0**): all configured checks passed; an unconfigured LLM is
  explicitly listed under `skipped` with a reason.
- `degraded` (exit **2**): a noncritical check failed (the standard LLM probe).
- `invalid` (exit **3**): a critical target, Caido, or recon check failed.
- Invalid CLI syntax/thresholds use exit **2** and print an error to stderr.

JSON fields: `verdict`, `failures` (name → reason), `checked_at` (UTC RFC3339),
optional `skipped` (name → reason). Verdict failures do not add stderr chatter in
JSON mode. Error messages exclude response bodies and credentials.

## Checks and configuration

- **target**: `GET` to the target origin's `/` (any supplied URL path is ignored),
  HTTP 200, `text/html` media type, body strictly larger than 1024 bytes.
- **caido**: authenticated `POST {caido-url}/graphql`, query
  `{projects{id name}}`. HTTP 200 alone is insufficient: `data.projects` must be
  a nonempty array of project records with IDs and names, without GraphQL errors.
  URL defaults to `CAIDO_URL`; bearer token comes from `CAIDO_ACCESS_TOKEN`.
- **llm**: one OpenAI-compatible `POST {llm-url}/chat/completions`, one user
  message, `max_tokens: 1`; requires a nonempty completion, not merely HTTP 200.
  Configure a cheap model with `--llm-url` (including `/v1`) and `--llm-model`, or
  `BB_ASSISTANT_LLM_URL` / `BB_ASSISTANT_LLM_MODEL`. API key is
  `BB_ASSISTANT_LLM_API_KEY`. Fallback variables: `OPENAI_BASE_URL`, `OPENAI_MODEL`,
  `OPENAI_API_KEY`. Local providers may omit the key. All config absent means
  skipped; partial config means degraded, not skipped. No default model is guessed.
- **recon**: `routes.json` and `input-points.json` in `--recon-dir`. Minimums are
  `--min-routes 20`, `--min-input-points 5`; both must be positive.

Recon files accept either JSON arrays or named envelopes:

```json
{"routes":[{"url":"/api/items","body_sha256":"a-body-hash","content_type":"application/json"}]}
```

```json
{"input_points":[{"url":"/api/items","name":"id"}]}
```

The input envelope may also use `input-points`. Entries must be nonempty objects
or nonempty strings (e.g. URL lists). Summary counts, missing/null artifacts,
malformed JSON, empty records, and files larger than 32 MiB fail closed.

If route objects have `body_sha256`, groups of identical HTML response hashes
(with `content_type` `text/html` or absent) are conservatively counted as SPA
shells. All members of repeated groups count, including multiple different shells;
a combined share **>= 50%** fails. Known non-HTML types are excluded. Without
fingerprints this heuristic cannot run; route/input minimums still apply. It is
not a semantic HTML classifier and can reject legitimately identical HTML pages.

Network requests do not follow redirects and default to 10-second timeouts;
`--timeout` bounds the overall command (default 30 seconds). JSON responses are
bounded to 4 MiB. Use only origins you are authorized to probe.

## Package use

Construct `gate.New(gate.TargetProbe(...), gate.CaidoProbe(...), ...).Check(ctx)`.
`Probe` exposes `Name`, `Critical`, and `Check func(context.Context) error` for
custom probes. `gate.Skip(reason)` explicitly skips an optional probe only;
critical skips are failures. An empty gate is invalid. Results are computed
fresh on every call, so the same gate can be reused after each stage.
