package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"bb-graph-assistant/internal/project"
)

// requireConfiguredProjectRegistry prevents read-only inventory commands from
// treating an unresolved implicit root as an empty project catalog.
func requireConfiguredProjectRegistry(root string) error {
	if root != "" {
		return nil
	}
	if value, ok := os.LookupEnv("BB_ASSISTANT_DATA_HOME"); ok && value != "" {
		return nil
	}
	dataRoot, err := project.DataRoot()
	if err != nil {
		return err
	}
	registryPath := filepath.Join(dataRoot, "projects", "registry.json")
	if _, err := os.Lstat(registryPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	serviceRoot := filepath.Join(dataRoot, "graph-kb-assistant")
	return fmt.Errorf(
		"BB_ASSISTANT_DATA_HOME is not set and no project registry was found at %s; the managed service data may be under %s. Set BB_ASSISTANT_DATA_HOME to the existing private runtime root (see docs/GO-CUTOVER-RUNBOOK.md)",
		registryPath,
		serviceRoot,
	)
}
