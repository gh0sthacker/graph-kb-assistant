"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const { evidenceVaultPresentation } = require("./assets/runtime-capabilities.js");

test("evidence status explains that a missing original key locks saved bytes", () => {
  const view = evidenceVaultPresentation("missing_or_invalid");
  assert.equal(view.label, "Evidence locked");
  assert.equal(view.state, "locked");
  assert.match(view.title, /original vault key/i);
  assert.match(view.title, /BB_ASSISTANT_VAULT_KEY/i);
  assert.match(view.title, /private runtime\.env/i);
  assert.match(view.title, /restart Graph Assistant/i);
  assert.match(view.title, /new key cannot decrypt/i);
});

test("configured key remains unverified until stored data decrypts", () => {
  const view = evidenceVaultPresentation("configured_not_verified");
  assert.equal(view.label, "Evidence key unverified");
  assert.equal(view.state, "unverified");
  assert.match(view.title, /not been verified/i);
});

test("unknown readiness data does not claim evidence access", () => {
  const view = evidenceVaultPresentation("unexpected");
  assert.equal(view.label, "Evidence status unknown");
  assert.equal(view.state, "unknown");
});
