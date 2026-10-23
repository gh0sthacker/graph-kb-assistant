package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"bb-graph-assistant/internal/projectlock"
)

const registryVersion = 1

type registryDocument struct {
	SchemaVersion int            `json:"schema_version"`
	Projects      []registryItem `json:"projects"`
}

// Field order matches the canonical, human-readable Python registry layout.
type registryItem struct {
	DisplayName *string `json:"display_name"`
	InstanceID  string  `json:"instance_id"`
	ProjectID   string  `json:"project_id"`
	ProjectKey  string  `json:"project_key"`
}

// RegistryPath returns the path-free identity catalog. Registry fields are
// never interpreted as path components.
func RegistryPath(root string) (string, error) {
	base, err := runtimeRoot(root)
	if err != nil {
		return "", err
	}
	projects, err := EnsurePrivateDirectory(filepath.Join(base, "projects"))
	if err != nil {
		return "", err
	}
	return filepath.Join(projects, "registry.json"), nil
}

// LoadProjectRefs reads and validates the version-1 identity catalog.
func LoadProjectRefs(root string) ([]CaidoProjectRef, error) {
	path, err := RegistryPath(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return []CaidoProjectRef{}, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("project registry is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("project registry cannot be read")
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, 16<<20+1))
	if err != nil || len(encoded) > 16<<20 {
		return nil, errors.New("project registry cannot be read")
	}

	var outer map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &outer); err != nil || !hasExactKeys(outer, "schema_version", "projects") {
		return nil, errors.New("project registry has an unsupported format")
	}
	var version int
	if err := json.Unmarshal(outer["schema_version"], &version); err != nil || version != registryVersion {
		return nil, errors.New("project registry has an unsupported format")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(outer["projects"], &rows); err != nil {
		return nil, errors.New("project registry has an unsupported format")
	}

	refs := make([]CaidoProjectRef, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, raw := range rows {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil || !hasExactKeys(row, "project_key", "instance_id", "project_id", "display_name") {
			return nil, errors.New("project registry contains a malformed row")
		}
		var item registryItem
		if err := json.Unmarshal(row["project_key"], &item.ProjectKey); err != nil {
			return nil, errors.New("project registry contains a malformed row")
		}
		if err := json.Unmarshal(row["instance_id"], &item.InstanceID); err != nil {
			return nil, errors.New("project registry contains a malformed row")
		}
		if err := json.Unmarshal(row["project_id"], &item.ProjectID); err != nil {
			return nil, errors.New("project registry contains a malformed row")
		}
		if !bytes.Equal(bytes.TrimSpace(row["display_name"]), []byte("null")) {
			var displayName string
			if err := json.Unmarshal(row["display_name"], &displayName); err != nil {
				return nil, errors.New("project registry contains a malformed row")
			}
			item.DisplayName = &displayName
		}
		ref := CaidoProjectRef{InstanceID: item.InstanceID, ProjectID: item.ProjectID, DisplayName: item.DisplayName}
		key, keyErr := ref.StorageKey()
		if keyErr != nil || key != item.ProjectKey {
			return nil, errors.New("project registry identity hash is inconsistent")
		}
		if _, ok := seen[key]; ok {
			return nil, errors.New("project registry identity hash is inconsistent")
		}
		seen[key] = struct{}{}
		refs = append(refs, ref)
	}
	sortProjectRefs(refs)
	return refs, nil
}

// RegisterProject atomically adds or updates one project identity.
func RegisterProject(ref CaidoProjectRef, root string) error {
	key, err := ref.StorageKey()
	if err != nil {
		return err
	}
	path, err := RegistryPath(root)
	if err != nil {
		return err
	}
	catalogLease, err := projectlock.Acquire(context.Background(), filepath.Dir(path))
	if err != nil {
		return errors.New("project catalog maintenance lease could not be acquired")
	}
	defer catalogLease.Close()
	refs, err := LoadProjectRefs(root)
	if err != nil {
		return err
	}
	byKey := make(map[string]CaidoProjectRef, len(refs)+1)
	for _, current := range refs {
		currentKey, keyErr := current.StorageKey()
		if keyErr != nil {
			return errors.New("project registry contains an invalid identity")
		}
		byKey[currentKey] = current
	}
	byKey[key] = ref
	refs = refs[:0]
	for _, current := range byKey {
		refs = append(refs, current)
	}
	sortProjectRefs(refs)
	return writeRegistry(root, refs)
}

func writeRegistry(root string, refs []CaidoProjectRef) error {
	path, err := RegistryPath(root)
	if err != nil {
		return err
	}
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("project registry must not be a symbolic link")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return errors.New("project registry cannot be updated")
	}
	items := make([]registryItem, 0, len(refs))
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return errors.New("project registry contains an invalid identity")
		}
		items = append(items, registryItem{
			DisplayName: ref.DisplayName,
			InstanceID:  ref.InstanceID,
			ProjectID:   ref.ProjectID,
			ProjectKey:  key,
		})
	}
	document := registryDocument{SchemaVersion: registryVersion, Projects: items}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return errors.New("project registry cannot be encoded")
	}

	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".registry-*.tmp")
	if err != nil {
		return errors.New("project registry cannot be updated")
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
		return errors.New("project registry permissions cannot be restricted")
	}
	if _, err := temporary.Write(buffer.Bytes()); err != nil {
		return errors.New("project registry cannot be updated")
	}
	if err := temporary.Sync(); err != nil {
		return errors.New("project registry cannot be synchronized")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("project registry cannot be updated")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.New("project registry cannot be updated")
	}
	renamed = true
	if err := os.Chmod(path, 0o600); err != nil {
		return errors.New("project registry permissions cannot be restricted")
	}
	if dir, openErr := os.Open(directory); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func runtimeRoot(root string) (string, error) {
	if root == "" {
		return DataRoot()
	}
	resolved, err := resolvePath(root)
	if err != nil {
		return "", errors.New("could not resolve private data root")
	}
	return resolved, nil
}

func hasExactKeys(value map[string]json.RawMessage, keys ...string) bool {
	if value == nil || len(value) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := value[key]; !ok {
			return false
		}
	}
	return true
}

func sortProjectRefs(refs []CaidoProjectRef) {
	sort.Slice(refs, func(i, j int) bool {
		left, right := "", ""
		if refs[i].DisplayName != nil {
			left = *refs[i].DisplayName
		}
		if refs[j].DisplayName != nil {
			right = *refs[j].DisplayName
		}
		if left != right {
			return left < right
		}
		leftKey, _ := refs[i].StorageKey()
		rightKey, _ := refs[j].StorageKey()
		return leftKey < rightKey
	})
}
