package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// LoadProjectRefsReadOnly reads the project catalog without creating or
// changing directories. It is intended for read-only CLI and assistant query
// paths; callers that need an initialized registry should keep using
// LoadProjectRefs/RegisterProject.
func LoadProjectRefsReadOnly(root string) ([]CaidoProjectRef, error) {
	base, err := runtimeRoot(root)
	if err != nil {
		return nil, err
	}
	projectsDir := filepath.Join(base, "projects")
	projectsInfo, err := os.Lstat(projectsDir)
	if errors.Is(err, os.ErrNotExist) {
		return []CaidoProjectRef{}, nil
	}
	if err != nil || projectsInfo.Mode()&os.ModeSymlink != 0 || !projectsInfo.IsDir() {
		return nil, errors.New("project registry directory is unavailable")
	}

	path := filepath.Join(projectsDir, "registry.json")
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
