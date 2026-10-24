package ingest

import (
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
