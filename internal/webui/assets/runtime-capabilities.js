(function (root) {
  "use strict";

  function evidenceVaultPresentation(state) {
    if (state === "missing_or_invalid") {
      return {
        label: "Evidence locked",
        title: "The local evidence viewer needs the original vault key. Restore BB_ASSISTANT_VAULT_KEY in the private runtime.env file and restart Graph Assistant; a new key cannot decrypt existing captures.",
        state: "locked",
      };
    }
    if (state === "configured_not_verified") {
      return {
        label: "Evidence key unverified",
        title: "A valid key format is configured. Decryption against stored captures has not been verified.",
        state: "unverified",
      };
    }
    if (state === "verified") {
      return {
        label: "Evidence key verified",
        title: "The key authenticated and checksum-verified a stored capture in this run. Other captures are verified individually when opened.",
        state: "verified",
      };
    }
    return {
      label: "Evidence status unknown",
      title: "The local evidence key status could not be checked.",
      state: "unknown",
    };
  }

  const api = { evidenceVaultPresentation };
  if (typeof module === "object" && module.exports) module.exports = api;
  else root.BBRuntimeCapabilities = api;
})(globalThis);
