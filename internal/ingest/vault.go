package ingest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"bb-graph-assistant/internal/caido"
)

const vaultMagic = "BBA1"

var ErrVaultKeyUnavailable = errors.New("local capture vault key is unavailable or invalid")

func readVaultKey() ([]byte, error) {
	encoded := os.Getenv("BB_ASSISTANT_VAULT_KEY")
	if encoded == "" || strings.Trim(encoded, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-=") != "" {
		return nil, ErrVaultKeyUnavailable
	}
	if strings.Contains(strings.TrimRight(encoded, "="), "=") || len(encoded)-len(strings.TrimRight(encoded, "=")) > 2 {
		return nil, ErrVaultKeyUnavailable
	}
	unpadded := strings.TrimRight(encoded, "=")
	key, err := base64.RawURLEncoding.DecodeString(unpadded)
	if err != nil || len(key) != 32 {
		return nil, ErrVaultKeyUnavailable
	}
	return key, nil
}

func writeVaultObject(directory, observationID, side string, raw []byte, key []byte) (string, string, error) {
	digestBytes := sha256.Sum256(raw)
	digest := hex.EncodeToString(digestBytes[:])
	path := filepath.Join(directory, observationID+"."+side+"."+digest+".bba")
	if info, err := os.Lstat(path); err == nil {
		return verifyExistingVaultObject(path, info, observationID, side, raw, digest, key)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", errors.New("vault object path is unavailable")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", errors.New("vault encryption could not be initialized")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", errors.New("vault encryption could not be initialized")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", "", errors.New("vault nonce could not be generated")
	}
	encoded := make([]byte, 0, len(vaultMagic)+len(nonce)+len(raw)+aead.Overhead())
	encoded = append(encoded, vaultMagic...)
	encoded = append(encoded, nonce...)
	encoded = aead.Seal(encoded, nonce, raw, vaultAAD(observationID, side))
	temp, err := os.CreateTemp(directory, ".capture-*")
	if err != nil {
		return "", "", errors.New("vault object could not be created")
	}
	tempName := temp.Name()
	cleanup := func() { _ = os.Remove(tempName) }
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		cleanup()
		return "", "", errors.New("vault object permissions could not be restricted")
	}
	if _, err := temp.Write(encoded); err != nil {
		_ = temp.Close()
		cleanup()
		return "", "", errors.New("vault object could not be written")
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		cleanup()
		return "", "", errors.New("vault object could not be synchronized")
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return "", "", errors.New("vault object could not be closed")
	}
	// Hard-link publication is atomic and refuses to replace an existing immutable object.
	if err := os.Link(tempName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			cleanup()
			info, statErr := os.Lstat(path)
			if statErr != nil {
				return "", "", errors.New("existing vault object is unavailable")
			}
			return verifyExistingVaultObject(path, info, observationID, side, raw, digest, key)
		}
		cleanup()
		return "", "", errors.New("vault object could not be published")
	}
	cleanup()
	dir, err := os.Open(directory)
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	if err != nil {
		return "", "", errors.New("vault directory could not be synchronized")
	}
	return filepath.Base(path), digest, nil
}

func verifyExistingVaultObject(path string, info os.FileInfo, observationID, side string, raw []byte, digest string, key []byte) (string, string, error) {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o600 != 0o600 {
		return "", "", errors.New("existing vault object is not a private regular file")
	}
	contents, readErr := readPrivateFile(path, maxPageBytes+64)
	if readErr != nil {
		return "", "", errors.New("existing vault object could not be read")
	}
	decoded, decodeErr := decryptVaultObject(contents, observationID, side, key)
	if decodeErr != nil || !equalBytes(decoded, raw) || sha256Hex(decoded) != digest {
		return "", "", errors.New("existing vault object failed integrity verification")
	}
	return filepath.Base(path), digest, nil
}

func decryptVaultObject(encoded []byte, observationID, side string, key []byte) ([]byte, error) {
	if len(encoded) < len(vaultMagic)+12+16 || string(encoded[:len(vaultMagic)]) != vaultMagic {
		return nil, errors.New("invalid vault object")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	start := len(vaultMagic)
	nonce := encoded[start : start+aead.NonceSize()]
	return aead.Open(nil, nonce, encoded[start+aead.NonceSize():], vaultAAD(observationID, side))
}

func vaultAAD(observationID, side string) []byte {
	return []byte(fmt.Sprintf("bb-graph-assistant:v1:%s:%s", observationID, side))
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validateExchange(exchange caido.HttpExchange, selectedProjectKey string) (string, error) {
	if err := exchange.Validate(); err != nil {
		return "", errors.New("history exchange is invalid")
	}
	key, err := exchange.Ref.Project.StorageKey()
	if err != nil || key != selectedProjectKey {
		return "", errors.New("history exchange belongs to another project")
	}
	observationID, err := exchange.Ref.ObservationID()
	if err != nil {
		return "", errors.New("history exchange identity is invalid")
	}
	if len(observationID) != 64 {
		return "", errors.New("history exchange identity is invalid")
	}
	if len(exchange.Method) > 128 || len(exchange.Scheme) > 16 || len(exchange.Host) > 253 || len(exchange.Target) > caido.TargetURLBytesMaximum {
		return "", errors.New("history exchange metadata exceeds its size limit")
	}
	if exchange.CapturedAt != nil && (len(*exchange.CapturedAt) > 128 || !utf8.ValidString(*exchange.CapturedAt)) {
		return "", errors.New("history exchange timestamp is invalid")
	}
	if exchange.SourceCursor != nil && (len(*exchange.SourceCursor) > maxCursorBytes || !utf8.ValidString(*exchange.SourceCursor)) {
		return "", errors.New("history source cursor is invalid")
	}
	return observationID, nil
}
