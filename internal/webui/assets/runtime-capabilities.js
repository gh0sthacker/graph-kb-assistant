(function (root) {
  "use strict";

  function evidenceVaultPresentation(state) {
    if (state === "missing_or_invalid") {
      return {
        label: "Evidence locked",
        title: "The local evidence viewer needs the original vault key. A new key cannot decrypt existing captures.",
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
