---
name: harness-provider-evaluation
description: Compare agent harnesses and model providers independently for the standalone bug-bounty assistant.
---

# Harness and model provider evaluation

Use this skill when selecting or integrating the assistant's harness, model, or
learning loop.

- Keep harness selection separate from model/provider selection. Hermes Agent
  is the selected runtime; the model/provider remains unset until the user
  chooses a route.
- Do not start OAuth or configure an unidentified third-party Bridge. Treat
  subscription access and API billing as separate routes and verify any chosen
  provider's data boundary before real capture data is used.
- Keep the app profile limited to the allowlisted read-only project MCP
  server. Do not enable Hermes terminal, file, browser, web, or code-execution
  toolsets for the app runtime.
- Load canonical project skills from .claude/skills/ as external skills and
  require approval for every skill write.
- Start with synthetic or redacted fixtures and a fixed scenario suite covering
  project selection, graph retrieval, KB citations, saved state, and resume.
- Compare tool-call correctness, schema validation, approval behavior,
  traceability, resume/recovery, latency, cost, and data flow.
- Inspect what the harness and model provider retain or log before sending real
  captures. Send only the minimum redacted evidence needed for the task.
- Tool calls never grant scope or authorization. Enforce allowlists, selected
  project boundaries, and operation policy in the coordinator.
- Learning systems may propose versioned changes. Evaluate those changes on a
  fixed fixture suite and review them before adoption; never let memory modify
  authorization, KB source policy, or finding status.
