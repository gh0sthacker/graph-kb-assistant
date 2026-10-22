// Package caido provides a read-only adapter for Caido's documented MCP tools.
// It never accesses Caido's internal database or exposes active testing tools.
package caido

import (
	identity "bb-graph-assistant/internal/project"
	"errors"
)

// The project and exchange identities are shared with storage and graph code.
type ProjectRef = identity.CaidoProjectRef
type ExchangeRef = identity.CaidoExchangeRef

var ErrMCP = errors.New("Caido MCP history read failed")

// ProjectRef is a stable local identity. Caido's current MCP tools do not
// expose a stable instance identifier, so InstanceID must be operator-set.
type HttpExchange struct {
	Ref                  ExchangeRef
	CapturedAt           *string
	RequestRaw           []byte
	ResponseRaw          []byte
	HasResponse          bool
	Method               string
	Scheme               string
	Host                 string
	Port                 int
	Target               string
	ResponseStatus       *int
	RequestTruncated     *bool
	ResponseTruncated    *bool
	SourceCursor         *string
	RequestWireFidelity  string
	ResponseWireFidelity string
}

func (e HttpExchange) Validate() error {
	if err := e.Ref.Validate(); err != nil {
		return err
	}
	if e.RequestRaw == nil {
		return errors.New("request_raw must be present")
	}
	if e.RequestWireFidelity != "source_bytes" && e.RequestWireFidelity != "reconstructed_text" && e.RequestWireFidelity != "unknown" {
		return errors.New("unsupported request wire fidelity")
	}
	if e.ResponseWireFidelity != "source_bytes" && e.ResponseWireFidelity != "reconstructed_text" && e.ResponseWireFidelity != "unknown" {
		return errors.New("unsupported response wire fidelity")
	}
	if !e.HasResponse && e.ResponseRaw != nil {
		return errors.New("response bytes set for an absent response")
	}
	return nil
}

type RequestMetadata struct {
	RequestID  string
	Method     string
	URL        string
	StatusCode *int
}

type RequestMetadataPage struct {
	Requests   []RequestMetadata
	NextCursor *string
	HasMore    bool
}

type HistoryPage struct {
	Exchanges  []HttpExchange
	NextCursor *string
	HasMore    bool
}

// ReadSource is the harness-neutral read contract used by import and tail.
type ReadSource interface {
	ListProjects() ([]ProjectRef, error)
	ListRequestPage(project ProjectRef, cursor *string, pageSize int, httpql *string) (RequestMetadataPage, error)
	FetchHistoryPage(project ProjectRef, page RequestMetadataPage, skipIDs map[string]struct{}, maxBodyBytes int) (HistoryPage, error)
	GetExchange(ref ExchangeRef) (*HttpExchange, error)
}

type safeError struct{ message string }

func (e safeError) Error() string { return e.message }

func sanitized(message string) error { return safeError{message: message} }
