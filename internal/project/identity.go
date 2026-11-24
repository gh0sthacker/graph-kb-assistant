// Package project contains harness-independent Caido project identities.
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// CaidoProjectRef is an opaque Caido instance/project identity. DisplayName is
// presentation metadata and is never used to construct a filesystem path.
type CaidoProjectRef struct {
	InstanceID  string  `json:"instance_id"`
	ProjectID   string  `json:"project_id"`
	DisplayName *string `json:"display_name"`
}

// Validate checks the same printable-ASCII identifier contract as the Python
// implementation. IDs are opaque and may not contain whitespace or controls.
func (p CaidoProjectRef) Validate() error {
	if !validOpaqueID(p.InstanceID) || !validOpaqueID(p.ProjectID) {
		return errors.New("Caido instance and project IDs must be non-empty opaque identifiers")
	}
	return nil
}

// SameProject compares the source identity, not its display name.
func (p CaidoProjectRef) SameProject(other CaidoProjectRef) bool {
	return p.InstanceID == other.InstanceID && p.ProjectID == other.ProjectID
}

// StorageKey returns the SHA-256 namespace key used by the Python project.
func (p CaidoProjectRef) StorageKey() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	canonical := `{"instance_id":` + quoteASCII(p.InstanceID) + `,"project_id":` + quoteASCII(p.ProjectID) + `}`
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}

// CaidoExchangeRef identifies one HTTP-history row within an instance/project.
type CaidoExchangeRef struct {
	Project   CaidoProjectRef `json:"project"`
	RequestID string          `json:"request_id"`
}

func (r CaidoExchangeRef) Validate() error {
	if err := r.Project.Validate(); err != nil {
		return err
	}
	if !validOpaqueID(r.RequestID) {
		return errors.New("Caido request ID must be a non-empty opaque identifier")
	}
	return nil
}

// ObservationID is compatible with the Python canonical JSON/SHA-256 identity.
func (r CaidoExchangeRef) ObservationID() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	canonical := `{"instance_id":` + quoteASCII(r.Project.InstanceID) +
		`,"project_id":` + quoteASCII(r.Project.ProjectID) +
		`,"request_id":` + quoteASCII(r.RequestID) + `}`
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}

func validOpaqueID(value string) bool {
	if len(value) < 1 || len(value) > 512 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

// quoteASCII emits the subset of JSON string escaping needed for valid IDs.
// Python json.dumps(..., ensure_ascii=False) does not escape HTML characters;
// encoding/json does, so using it here would produce incompatible hashes.
func quoteASCII(value string) string {
	var out strings.Builder
	out.Grow(len(value) + 2)
	out.WriteByte('"')
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteByte(value[i])
		default:
			out.WriteByte(value[i])
		}
	}
	out.WriteByte('"')
	return out.String()
}

func (p CaidoProjectRef) String() string {
	key, err := p.StorageKey()
	if err != nil {
		return "<invalid Caido project>"
	}
	return fmt.Sprintf("CaidoProjectRef{%s}", key)
}
