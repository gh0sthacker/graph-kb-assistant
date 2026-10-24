package ingest

import (
	"encoding/base64"
	"errors"
	"testing"
)

func TestReadVaultKeyReportsUnavailableWithoutReturningKeyMaterial(t *testing.T) {
	for _, test := range []struct{ name, value string }{
		{name: "missing", value: ""},
		{name: "malformed", value: "not-a-key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("BB_ASSISTANT_VAULT_KEY", test.value)
			key, err := readVaultKey()
			if len(key) != 0 || !errors.Is(err, ErrVaultKeyUnavailable) {
				t.Fatalf("readVaultKey() returned %d key bytes, error %v; want no key and typed unavailable error", len(key), err)
			}
		})
	}
}

func TestVaultKeyConfigurationStateNeverClaimsDecryptionWasVerified(t *testing.T) {
	t.Setenv("BB_ASSISTANT_VAULT_KEY", "")
	if got := VaultKeyConfigurationState(); got != vaultKeyStateMissingOrInvalid {
		t.Fatalf("missing key state = %q", got)
	}
	key := make([]byte, 32)
	encoded := base64.RawURLEncoding.EncodeToString(key)
	t.Setenv("BB_ASSISTANT_VAULT_KEY", encoded)
	if got := VaultKeyConfigurationState(); got != vaultKeyStateConfiguredNotVerified {
		t.Fatalf("configured key state = %q", got)
	}
}
