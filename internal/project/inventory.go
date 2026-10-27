package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"bb-graph-assistant/internal/projectlock"
)

const inventoryStateVersion = 1

type inventoryState struct {
	SchemaVersion int            `json:"schema_version"`
	Missing       map[string]int `json:"missing_confirmations"`
}

// ObserveProjectInventory records one complete, successful Caido inventory
// response. A project is returned only after the configured number of
// successful responses from the same instance omitted it. API failures must
// not call this function. Poll cadence is owned by the sync manager.
func ObserveProjectInventory(ctx context.Context, root, instanceID string, available []CaidoProjectRef, confirmations int) ([]CaidoProjectRef, error) {
	if ctx == nil || instanceID == "" || confirmations < 2 || confirmations > 20 {
		return nil, errors.New("project inventory confirmation policy is invalid")
	}
	availableKeys := make(map[string]struct{}, len(available))
	for _, ref := range available {
		if ref.InstanceID != instanceID || ref.Validate() != nil {
			return nil, errors.New("project inventory contains an invalid identity")
		}
		key, err := ref.StorageKey()
		if err != nil {
			return nil, errors.New("project inventory contains an invalid identity")
		}
		if _, duplicate := availableKeys[key]; duplicate {
			return nil, errors.New("project inventory contains duplicate identities")
		}
		availableKeys[key] = struct{}{}
	}

	registryPath, err := RegistryPath(root)
	if err != nil {
		return nil, err
	}
	catalogDir := filepath.Dir(registryPath)
	lease, err := projectlock.Acquire(ctx, catalogDir)
	if err != nil {
		return nil, errors.New("project catalog maintenance lease could not be acquired")
	}
	defer lease.Close()
	registered, err := LoadProjectRefs(root)
	if err != nil {
		return nil, err
	}
	state, err := readInventoryState(filepath.Join(catalogDir, "inventory-state.json"))
	if err != nil {
		return nil, err
	}
	missing := make([]CaidoProjectRef, 0)
	for _, ref := range registered {
		if ref.InstanceID != instanceID {
			continue
		}
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return nil, errors.New("project registry contains an invalid identity")
		}
		if _, present := availableKeys[key]; present {
			delete(state.Missing, key)
			continue
		}
		if state.Missing[key] < confirmations {
			state.Missing[key]++
			// Emit only the transition to absent. The project stays registered
			// and its evidence remains available as history; a returning project
			// resets this counter above and can resume synchronization.
			if state.Missing[key] == confirmations {
				missing = append(missing, ref)
			}
		}
	}
	if err := writeInventoryState(filepath.Join(catalogDir, "inventory-state.json"), state); err != nil {
		return nil, err
	}
	return missing, nil
}

func readInventoryState(path string) (inventoryState, error) {
	state := inventoryState{SchemaVersion: inventoryStateVersion, Missing: make(map[string]int)}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return inventoryState{}, errors.New("project inventory state is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return inventoryState{}, errors.New("project inventory state cannot be read")
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(encoded) > 1<<20 {
		return inventoryState{}, errors.New("project inventory state cannot be read")
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &outer); err != nil || !hasExactKeys(outer, "schema_version", "missing_confirmations") {
		return inventoryState{}, errors.New("project inventory state has an unsupported format")
	}
	if err := json.Unmarshal(outer["schema_version"], &state.SchemaVersion); err != nil || state.SchemaVersion != inventoryStateVersion {
		return inventoryState{}, errors.New("project inventory state has an unsupported format")
	}
	if err := json.Unmarshal(outer["missing_confirmations"], &state.Missing); err != nil || state.Missing == nil {
		return inventoryState{}, errors.New("project inventory state has an unsupported format")
	}
	for key, count := range state.Missing {
		if !validStorageKey(key) || count < 1 || count > 20 {
			return inventoryState{}, errors.New("project inventory state has an invalid entry")
		}
	}
	return state, nil
}

func writeInventoryState(path string, state inventoryState) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		return errors.New("project inventory state cannot be encoded")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".inventory-*.tmp")
	if err != nil {
		return errors.New("project inventory state cannot be updated")
	}
	temporaryPath := temporary.Name()
	renamed := false
	defer func() {
		_ = temporary.Close()
		if !renamed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.New("project inventory state permissions cannot be restricted")
	}
	if _, err := temporary.Write(buffer.Bytes()); err != nil {
		return errors.New("project inventory state cannot be updated")
	}
	if err := temporary.Sync(); err != nil {
		return errors.New("project inventory state cannot be synchronized")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("project inventory state cannot be updated")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.New("project inventory state cannot be updated")
	}
	renamed = true
	if err := os.Chmod(path, 0o600); err != nil {
		return errors.New("project inventory state permissions cannot be restricted")
	}
	if dir, openErr := os.Open(filepath.Dir(path)); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func validStorageKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, r := range key {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
