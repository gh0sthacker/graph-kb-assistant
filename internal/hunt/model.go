// Package hunt projects captured evidence into deterministic hunting hypotheses.
// A candidate is not a finding. No network requests or model calls are made.
package hunt

type Capture struct {
	ID, ObservationID, RevisionID, Namespace string
	Host, Scheme                             string
	Port                                     int
	Request, Response                        []byte
}
type Endpoint struct {
	ID            string `json:"id"`
	Method        string `json:"method"`
	Route         string `json:"route_template"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	AuthRequired  bool   `json:"auth_required"`
	StateChanging bool   `json:"state_changing"`
}
type Parameter struct {
	ID                string   `json:"id"`
	EndpointID        string   `json:"endpoint_id"`
	Name              string   `json:"name"`
	Location          string   `json:"location"`
	ObservedTypes     []string `json:"observed_types"`
	Samples           []string `json:"samples"`
	DistinctCount     int      `json:"distinct_count"`
	Reflected         bool     `json:"reflected"`
	ReflectionContext string   `json:"reflection_context,omitempty"`
	// Digests support exact distinct counts without persisting secrets.
	ValueDigests []string `json:"value_digests"`
}
type Sink struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Engine       string  `json:"engine"`
	EvidenceKind string  `json:"evidence_kind"`
	Confidence   float64 `json:"confidence"`
	Signature    string  `json:"signature"`
}
type Identity struct {
	ID           string   `json:"id"`
	Label        string   `json:"label"`
	TokenRef     string   `json:"token_ref"`
	Capabilities []string `json:"capabilities"`
}
type Info struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Where    string `json:"where"`
	Redacted bool   `json:"redacted"`
	Value    string `json:"value"`
}
type Observation struct {
	CaptureID     string      `json:"capture_id"`
	ObservationID string      `json:"observation_id"`
	RevisionID    string      `json:"revision_id"`
	Endpoint      Endpoint    `json:"endpoint"`
	Parameters    []Parameter `json:"parameters"`
	Sinks         []Sink      `json:"sinks"`
	Identity      Identity    `json:"identity"`
	Infos         []Info      `json:"infos"`
	Status        int         `json:"status"`
}
type Edge struct {
	CandidateID string   `json:"candidate_id"`
	From        string   `json:"from"`
	To          string   `json:"to"`
	Kind        string   `json:"kind"`
	Evidence    []string `json:"evidence"`
	Status      int      `json:"status,omitempty"`
}
type Auth struct {
	Required bool `json:"required"`
}
type Candidate struct {
	ID             string   `json:"id"`
	Class          string   `json:"class"`
	Endpoint       string   `json:"endpoint"`
	EndpointID     string   `json:"endpoint_id"`
	Parameter      string   `json:"parameter"`
	Location       string   `json:"location"`
	Sink           Sink     `json:"sink"`
	Why            []string `json:"why"`
	Auth           Auth     `json:"auth"`
	Novelty        string   `json:"novelty"`
	Playbook       string   `json:"playbook"`
	SuggestedCheck string   `json:"suggested_check"`
	Score          float64  `json:"score"`
}
type Gap struct {
	Class  string `json:"class"`
	Reason string `json:"reason"`
}
type Stats struct {
	Endpoints    int `json:"endpoints"`
	Parameters   int `json:"parameters"`
	Sinks        int `json:"sinks"`
	Identities   int `json:"identities"`
	Infos        int `json:"infos"`
	Observations int `json:"observations"`
	Candidates   int `json:"candidates"`
}
type Result struct {
	Candidates   []Candidate `json:"candidates"`
	Uncovered    []Gap       `json:"uncovered"`
	Stats        Stats       `json:"stats"`
	CoverageGaps []string    `json:"coverage_gaps"`
}
type Graph struct {
	Endpoints  []Endpoint
	Parameters []Parameter
	Sinks      []Sink
	Identities []Identity
	Infos      []Info
	Edges      []Edge
	Result     Result
}
