// Package runtimeaudit builds a bounded, local-only capability and project
// status summary for the managed Go runtime.
package runtimeaudit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bb-graph-assistant/internal/graphaccess"
	"bb-graph-assistant/internal/neo4jstore"
	"bb-graph-assistant/internal/project"
	projectaudit "bb-graph-assistant/internal/projectaudit"
)

type GraphBackendStatus struct {
	Name          string `json:"name"`
	Configuration string `json:"configuration"`
	Connectivity  string `json:"connectivity"`
}

type CapabilitySummary struct {
	WebUI              string `json:"web_ui"`
	MCPStdio           string `json:"mcp_stdio"`
	Consultation       string `json:"consultation"`
	CaidoLiveIntake    string `json:"caido_live_intake"`
	ModelExecution     string `json:"model_execution"`
	CapturedBodiesRead bool   `json:"captured_bodies_read"`
}

type ProjectSummary struct {
	ProjectKey                 string                             `json:"project_key"`
	AuditState                 string                             `json:"audit_state"`
	CaptureStoreState          string                             `json:"capture_store_state"`
	CaptureSchemaVersion       int                                `json:"capture_schema_version,omitempty"`
	ExpectedCaptureSchema      int                                `json:"expected_capture_schema"`
	BehaviorGraphState         string                             `json:"behavior_graph_state"`
	BehaviorGraphSchemaVersion int                                `json:"behavior_graph_schema_version,omitempty"`
	CaptureCount               int64                              `json:"capture_count"`
	LiveIndexedCount           int64                              `json:"live_indexed_count"`
	GraphMappedCaptureCount    int64                              `json:"graph_mapped_capture_count"`
	UnmappedCaptureCount       int64                              `json:"unmapped_capture_count"`
	CoverageState              string                             `json:"coverage_state"`
	CoverageGaps               []string                           `json:"coverage_gaps"`
	HistoricalSyncState        string                             `json:"historical_sync_state"`
	LiveSyncState              string                             `json:"live_sync_state"`
	HistoryReconciled          bool                               `json:"history_reconciled"`
	ProjectionQueue            projectaudit.ProjectionQueueStatus `json:"projection_queue"`
}

type Report struct {
	GeneratedAt          time.Time          `json:"generated_at"`
	OverallState         string             `json:"overall_state"`
	ProjectRegistryState string             `json:"project_registry_state"`
	GraphBackend         GraphBackendStatus `json:"graph_backend"`
	Capabilities         CapabilitySummary  `json:"capabilities"`
	RegisteredProjects   int                `json:"registered_projects"`
	AuditedProjects      int                `json:"audited_projects"`
	UnavailableProjects  int                `json:"unavailable_projects"`
	UninspectedProjects  int                `json:"uninspected_projects"`
	Projects             []ProjectSummary   `json:"projects"`
	Notes                []string           `json:"notes"`
}

// Build reads project registry and structural status only. It does not access
// Caido, Neo4j, the encrypted vault, a model provider, or capture payloads.
func Build(ctx context.Context, dataRoot, backend string, generatedAt time.Time) (Report, error) {
	if ctx == nil || generatedAt.IsZero() {
		return Report{}, errors.New("runtime status context and timestamp are required")
	}
	refs, err := project.LoadProjectRefsReadOnly(dataRoot)
	if err != nil {
		return Report{}, errors.New("local project registry is unavailable")
	}
	backendStatus := describeGraphBackend(backend)
	report := Report{
		GeneratedAt:          generatedAt.UTC(),
		OverallState:         "partial",
		ProjectRegistryState: inspectProjectRegistryState(dataRoot),
		GraphBackend:         backendStatus,
		Capabilities: CapabilitySummary{
			WebUI:              "implemented; process state not checked",
			MCPStdio:           "implemented; harness connection not checked",
			Consultation:       "implemented; synthetic workflow verified",
			CaidoLiveIntake:    "not checked; Caido was not contacted",
			ModelExecution:     "not invoked",
			CapturedBodiesRead: false,
		},
		RegisteredProjects: len(refs),
		Projects:           make([]ProjectSummary, 0, len(refs)),
		Notes: []string{
			"This report contains local structural metadata and aggregate counts only; it does not read captured bodies or encrypted vault files.",
			"Graph backend connectivity, Caido intake, model-provider behavior, and browser/JavaScript coverage are not assessed by this command.",
			"Overall state remains partial until source history is reconciled and outstanding coverage gaps are verified.",
		},
	}
	if backendStatus.Configuration == "incomplete" || backendStatus.Configuration == "invalid" {
		report.Notes = append(report.Notes, "Graph backend configuration is incomplete or unsupported; no connection was attempted.")
	}
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return Report{}, errors.New("project registry contains an invalid identity")
		}
		summary := ProjectSummary{
			ProjectKey:         key,
			AuditState:         "not_inspected",
			CaptureStoreState:  "not_inspected",
			BehaviorGraphState: "not_inspected",
			CoverageState:      "not_assessed",
			ProjectionQueue:    projectaudit.ProjectionQueueStatus{State: "not_inspected"},
		}
		if ctx.Err() != nil {
			report.UnavailableProjects++
			report.Projects = append(report.Projects, summary)
			continue
		}
		audit, auditErr := projectaudit.AuditProject(ctx, dataRoot, key)
		if auditErr != nil {
			summary.AuditState = "unavailable"
			summary.CaptureStoreState = "unavailable"
			summary.BehaviorGraphState = "not_checked"
			summary.CoverageState = "partial"
			summary.ProjectionQueue = projectaudit.ProjectionQueueStatus{State: "unavailable"}
			report.Notes = append(report.Notes, "One or more project audits were unavailable; raw error details are omitted.")
			report.UninspectedProjects++
			report.Projects = append(report.Projects, summary)
			continue
		}
		summary.AuditState = audit.OverallState
		summary.CaptureStoreState = audit.CaptureStore.State
		summary.CaptureSchemaVersion = audit.CaptureStore.SchemaVersion
		summary.ExpectedCaptureSchema = audit.CaptureStore.ExpectedSchemaVersion
		summary.BehaviorGraphState = audit.BehaviorGraph.State
		summary.BehaviorGraphSchemaVersion = audit.BehaviorGraph.SchemaVersion
		summary.CaptureCount = audit.Coverage.CaptureCount
		summary.LiveIndexedCount = audit.Coverage.LiveIndexedCount
		summary.GraphMappedCaptureCount = audit.Coverage.GraphMappedCaptureCount
		summary.UnmappedCaptureCount = audit.Coverage.UnmappedCaptureCount
		summary.CoverageState = audit.Coverage.CoverageState
		summary.CoverageGaps = append([]string(nil), audit.Coverage.CoverageGaps...)
		summary.HistoricalSyncState = audit.Coverage.HistoricalSyncState
		summary.LiveSyncState = audit.Coverage.LiveSyncState
		summary.HistoryReconciled = audit.HistoryReconciled
		summary.ProjectionQueue = audit.ProjectionQueue
		report.AuditedProjects++
		report.Projects = append(report.Projects, summary)
	}
	return report, nil
}

func inspectProjectRegistryState(dataRoot string) string {
	root := dataRoot
	if root == "" {
		resolved, err := project.DataRoot()
		if err != nil {
			return "unavailable"
		}
		root = resolved
	}
	root = expandDataRoot(root)
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "unavailable"
	}
	projectsDir := filepath.Join(root, "projects")
	projectsInfo, err := os.Lstat(projectsDir)
	if errors.Is(err, os.ErrNotExist) {
		return "not_initialized"
	}
	if err != nil || projectsInfo.Mode()&os.ModeSymlink != 0 || !projectsInfo.IsDir() {
		return "unavailable"
	}
	registryInfo, err := os.Lstat(filepath.Join(projectsDir, "registry.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "empty"
	}
	if err != nil || registryInfo.Mode()&os.ModeSymlink != 0 || !registryInfo.Mode().IsRegular() {
		return "unavailable"
	}
	return "readable"
}

func expandDataRoot(root string) string {
	if root == "~" || strings.HasPrefix(root, "~"+string(filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if root == "~" {
				return home
			}
			return filepath.Join(home, root[2:])
		}
	}
	return root
}

func describeGraphBackend(backend string) GraphBackendStatus {
	status := GraphBackendStatus{Name: backend, Configuration: "invalid", Connectivity: "not_checked"}
	switch backend {
	case "sqlite":
		status.Configuration = "not_required"
		status.Connectivity = "not_applicable"
	case "neo4j":
		if _, err := neo4jstore.ConfigFromEnv(); err == nil {
			status.Configuration = "configured"
		} else {
			status.Configuration = "incomplete"
		}
	default:
		status.Name = "unsupported"
	}
	return status
}

// ConfiguredGraphBackend resolves the same default used by graph reads.
func ConfiguredGraphBackend() string { return graphaccess.ConfiguredBackend() }
