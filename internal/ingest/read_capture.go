package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

const maxRawCaptureRange = 16 << 10

// CaptureRange contains only the requested decrypted bytes, never a full
// plaintext copy of the stored message.
type CaptureRange struct {
	Data              []byte
	TotalSize         int64
	SHA256            string
	RevisionID        *string
	CapturedAt        *string
	ParseState        string
	RequestTruncated  *bool
	ResponseTruncated *bool
	WireFidelity      string
}

// ReadCaptureRange decrypts and authenticates one exact historical message
// from the explicitly selected project's private vault, then returns a bounded
// byte range. Live-tail rows are not copied to the local vault.
func ReadCaptureRange(ctx context.Context, projectStore *store.ProjectStore, requestID, side string, offset int64, maximum int) (CaptureRange, error) {
	if projectStore == nil || projectStore.Database == nil || projectStore.Database.DB() == nil || !projectStore.Database.ReadOnly() {
		return CaptureRange{}, errors.New("raw capture requires a read-only selected project store")
	}
	projectKey, keyErr := projectStore.Project.StorageKey()
	if keyErr != nil || projectKey != projectStore.ProjectKey || projectStore.Directory == "" ||
		projectStore.VaultDirectory != filepath.Join(projectStore.Directory, "vault") {
		return CaptureRange{}, errors.New("raw capture project namespace is inconsistent")
	}
	if side != "request" && side != "response" {
		return CaptureRange{}, errors.New("raw capture side is invalid")
	}
	if offset < 0 || maximum < 1 || maximum > maxRawCaptureRange || requestID == "" || len(requestID) > 512 {
		return CaptureRange{}, errors.New("raw capture range is outside its allowed bounds")
	}
	selectedRef := project.CaidoExchangeRef{Project: projectStore.Project, RequestID: requestID}
	observationID, err := selectedRef.ObservationID()
	if err != nil {
		return CaptureRange{}, errors.New("raw capture identity is invalid")
	}
	var row struct {
		observationID                                               string
		requestHash, responseHash, revision, capturedAt, parseState sql.NullString
		requestSize, responseSize                                   sql.NullInt64
		requestRef, responseRef                                     sql.NullString
		requestTruncated, responseTruncated                         sql.NullInt64
		requestFidelity, responseFidelity                           sql.NullString
	}
	err = projectStore.Database.DB().QueryRowContext(ctx, `
		SELECT c.observation_id,cr.request_sha256,cr.response_sha256,cr.request_size,cr.response_size,
		       cr.request_vault_ref,cr.response_vault_ref,cr.revision_id,cr.captured_at,c.parse_state,
	       cr.request_truncated,cr.response_truncated,cr.request_wire_fidelity,cr.response_wire_fidelity
		FROM captures AS c
		JOIN capture_revisions AS cr ON cr.revision_id=c.current_revision_id AND cr.observation_id=c.observation_id
		WHERE c.caido_instance_id=? AND c.caido_project_id=? AND c.request_id=?`,
		projectStore.Project.InstanceID, projectStore.Project.ProjectID, requestID,
	).Scan(&row.observationID, &row.requestHash, &row.responseHash, &row.requestSize, &row.responseSize,
		&row.requestRef, &row.responseRef, &row.revision, &row.capturedAt, &row.parseState,
		&row.requestTruncated, &row.responseTruncated, &row.requestFidelity, &row.responseFidelity)
	if err != nil {
		return CaptureRange{}, errors.New("historical raw capture was not found in the selected project")
	}
	if row.observationID != observationID || !row.revision.Valid || row.revision.String == "" {
		return CaptureRange{}, errors.New("historical raw capture identity is inconsistent")
	}
	if !row.requestHash.Valid || !row.requestSize.Valid || !row.requestRef.Valid ||
		row.requestSize.Int64 < 0 || row.requestSize.Int64 > maxPageBytes ||
		row.responseHash.Valid != row.responseSize.Valid || row.responseHash.Valid != row.responseRef.Valid {
		return CaptureRange{}, errors.New("historical raw capture revision is incomplete")
	}
	responseSize := nullableIntPointer(row.responseSize)
	if responseSize != nil && (*responseSize < 0 || *responseSize > maxPageBytes) {
		return CaptureRange{}, errors.New("historical raw capture revision exceeds the local read limit")
	}
	expectedRevision, err := captureRevisionID(
		observationID, row.requestHash.String, nullableStringPointer(row.responseHash), int(row.requestSize.Int64), responseSize,
		stringPointer(row.capturedAt), nullableStoredBool(row.requestTruncated), nullableStoredBool(row.responseTruncated),
		row.requestFidelity.String, row.responseFidelity.String,
	)
	if err != nil || expectedRevision != row.revision.String {
		return CaptureRange{}, errors.New("historical raw capture revision failed identity verification")
	}
	digest, vaultRef, size := row.requestHash, row.requestRef, row.requestSize
	fidelity := row.requestFidelity
	if side == "response" {
		digest, vaultRef, size = row.responseHash, row.responseRef, row.responseSize
		fidelity = row.responseFidelity
	}
	if !digest.Valid || !vaultRef.Valid || !size.Valid || size.Int64 < 0 || size.Int64 > maxPageBytes {
		return CaptureRange{}, errors.New("selected capture side is absent or exceeds the local read limit")
	}
	if offset > size.Int64 {
		return CaptureRange{}, errors.New("raw capture offset is beyond the message")
	}
	if len(digest.String) != 64 || strings.Trim(digest.String, "0123456789abcdef") != "" {
		return CaptureRange{}, errors.New("raw capture hash metadata is invalid")
	}
	expectedName := fmt.Sprintf("%s.%s.%s.bba", observationID, side, digest.String)
	if filepath.Base(vaultRef.String) != vaultRef.String || vaultRef.String != expectedName || strings.ContainsAny(vaultRef.String, `/\`) {
		return CaptureRange{}, errors.New("raw capture vault reference is invalid")
	}
	path := filepath.Join(projectStore.VaultDirectory, expectedName)
	if filepath.Dir(path) != projectStore.VaultDirectory {
		return CaptureRange{}, errors.New("raw capture vault path is invalid")
	}
	projectInfo, projectErr := os.Lstat(projectStore.Directory)
	vaultInfo, vaultErr := os.Lstat(projectStore.VaultDirectory)
	if projectErr != nil || projectInfo.Mode()&os.ModeSymlink != 0 || !projectInfo.IsDir() ||
		vaultErr != nil || vaultInfo.Mode()&os.ModeSymlink != 0 || !vaultInfo.IsDir() {
		return CaptureRange{}, errors.New("raw capture project vault is unavailable")
	}
	fileInfo, err := os.Lstat(path)
	if err != nil || fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() ||
		fileInfo.Mode().Perm()&0o077 != 0 || fileInfo.Mode().Perm()&0o600 != 0o600 || fileInfo.Size() > maxPageBytes+64 {
		return CaptureRange{}, errors.New("raw capture vault object is not a private regular file")
	}
	key, err := readVaultKey()
	if err != nil {
		return CaptureRange{}, errors.New("encrypted capture vault key is unavailable")
	}
	defer clear(key)
	if err := ctx.Err(); err != nil {
		return CaptureRange{}, errors.New("raw capture read was interrupted")
	}
	encoded, err := readPrivateFile(path, maxPageBytes+64)
	if err != nil {
		return CaptureRange{}, errors.New("raw capture vault object could not be read")
	}
	defer clear(encoded)
	openedInfo, err := os.Lstat(path)
	if err != nil || !os.SameFile(fileInfo, openedInfo) {
		return CaptureRange{}, errors.New("raw capture vault object changed during validation")
	}
	if err := ctx.Err(); err != nil {
		return CaptureRange{}, errors.New("raw capture read was interrupted")
	}
	raw, err := decryptVaultObject(encoded, observationID, side, key)
	if err != nil {
		return CaptureRange{}, errors.New("raw capture vault authentication failed")
	}
	defer clear(raw)
	if int64(len(raw)) != size.Int64 || sha256Hex(raw) != digest.String {
		return CaptureRange{}, errors.New("raw capture vault content failed integrity verification")
	}
	start := int(offset)
	end := min(start+maximum, len(raw))
	block := append([]byte(nil), raw[start:end]...)
	return CaptureRange{
		Data: block, TotalSize: size.Int64, SHA256: digest.String,
		RevisionID: stringPointer(row.revision), CapturedAt: stringPointer(row.capturedAt),
		ParseState:        defaultString(row.parseState.String, "not_parsed"),
		RequestTruncated:  nullableStoredBool(row.requestTruncated),
		ResponseTruncated: nullableStoredBool(row.responseTruncated),
		WireFidelity:      defaultString(fidelity.String, "unknown"),
	}, nil
}

func nullableStoredBool(value sql.NullInt64) *bool {
	if !value.Valid {
		return nil
	}
	result := value.Int64 != 0
	return &result
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func stringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
}

func nullableStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
}

func nullableIntPointer(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	result := int(value.Int64)
	return &result
}
