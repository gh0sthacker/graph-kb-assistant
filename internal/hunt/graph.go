package hunt

import (
	"errors"
	"fmt"
	"mime"
	"path"
	"sort"
	"strings"
)

var Classes = []string{"sqli", "nosql", "xss", "ssrf", "open-redirect", "ssti", "path-traversal", "idor", "bfla", "bopla", "business-logic", "eval/js", "deser", "input-validation"}

func allowsXSS(o Observation) bool {
	mediaType, _, err := mime.ParseMediaType(o.ResponseContentType)
	if err != nil || !strings.EqualFold(mediaType, "text/html") {
		return false
	}
	switch strings.ToLower(path.Ext(o.Endpoint.Route)) {
	case ".js", ".json", ".css", ".map":
		return false
	default:
		return true
	}
}

// ClassFit is the ADR name-to-class table. Exact names and their nested JSON
// leaf names score 1; other observed inputs score 0.5, never an invented sink.
func ClassFit(name, class string) float64 {
	name = strings.ToLower(name)
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, "[]")
	table := map[string][]string{"id": {"idor", "bfla"}, "uid": {"idor", "bfla"}, "user": {"idor", "bfla"}, "q": {"sqli", "nosql", "xss"}, "search": {"sqli", "nosql", "xss"}, "filter": {"sqli", "nosql", "xss"}, "url": {"ssrf", "open-redirect"}, "redirect": {"ssrf", "open-redirect"}, "next": {"ssrf", "open-redirect"}, "return": {"ssrf", "open-redirect"}, "template": {"ssti"}, "tpl": {"ssti"}, "file": {"path-traversal"}, "path": {"path-traversal"}, "name": {"path-traversal"}, "price": {"business-logic"}, "qty": {"business-logic"}, "amount": {"business-logic"}, "coupon": {"business-logic"}, "role": {"bopla", "bfla"}}
	for _, c := range table[name] {
		if class == c {
			return 1
		}
	}
	return 0.5
}
func score(confidence float64, reflected bool, fit, identity float64, novelty string, stateChanging bool, class string) float64 {
	reflectionBonus := 0.0
	if reflected {
		reflectionBonus = 1
	}
	n := 1.0
	if novelty == "tested" {
		n = 0.3
	}
	state := 1.0
	if stateChanging && (class == "business-logic" || class == "bfla") {
		state = 1.5
	}
	return confidence * (0.5 + 0.5*reflectionBonus) * fit * identity * n * state
}
func evidence(o Observation) []string {
	if o.CaptureID == "" {
		return nil
	}
	refs := []string{"capture:" + o.CaptureID}
	if o.ObservationID != "" {
		refs = append(refs, "observation:"+o.ObservationID)
	}
	if o.RevisionID != "" {
		refs = append(refs, "revision:"+o.RevisionID)
	}
	return refs
}
func union(a, b []string) []string {
	m := map[string]bool{}
	for _, v := range a {
		m[v] = true
	}
	for _, v := range b {
		m[v] = true
	}
	return sortedKeys(m)
}
func synthetic(kind, engine, reason string, confidence float64, endpoint string) Sink {
	return Sink{ID: node("sink", endpoint+"|"+kind+"|"+engine), Kind: kind, Engine: engine, Signature: reason, EvidenceKind: "observation", Confidence: confidence}
}

// Build merges only evidence-bearing observations. Co-observed errors are
// hypotheses about reachability, not a taint proof or a confirmed finding.
func Build(observations []Observation) Graph {
	observations = append([]Observation(nil), observations...)
	sort.SliceStable(observations, func(i, j int) bool {
		a, b := observations[i], observations[j]
		if a.CaptureID != b.CaptureID {
			return a.CaptureID < b.CaptureID
		}
		return a.RevisionID < b.RevisionID
	})
	g := Graph{Endpoints: []Endpoint{}, Parameters: []Parameter{}, Sinks: []Sink{}, Identities: []Identity{}, Infos: []Info{}, Edges: []Edge{}, Result: Result{Candidates: []Candidate{}, Uncovered: []Gap{}, CoverageGaps: []string{}}}
	endpoints := map[string]Endpoint{}
	params := map[string]Parameter{}
	sinks := map[string]Sink{}
	identities := map[string]Identity{}
	infos := map[string]Info{}
	candidates := map[string]Candidate{}
	edges := map[string]Edge{}
	denied := map[string][]string{}
	anonOK := map[string]bool{}
	valid := []Observation{}
	for _, o := range observations {
		if len(evidence(o)) == 0 || o.Endpoint.ID == "" {
			continue
		}
		valid = append(valid, o)
		endpoints[o.Endpoint.ID] = o.Endpoint
		identities[o.Identity.ID] = o.Identity
		if o.Identity.TokenRef == "anon" {
			if o.Status == 401 || o.Status == 403 {
				denied[o.Endpoint.ID] = union(denied[o.Endpoint.ID], evidence(o))
			}
			if o.Status >= 200 && o.Status < 300 {
				anonOK[o.Endpoint.ID] = true
			}
		}
	}
	addEdge := func(e Edge) {
		if len(e.Evidence) == 0 {
			return
		}
		key := e.CandidateID + "|" + e.From + "|" + e.To + "|" + e.Kind + fmt.Sprint(e.Status)
		if old, ok := edges[key]; ok {
			e.Evidence = union(e.Evidence, old.Evidence)
		}
		edges[key] = e
	}
	for id, e := range endpoints {
		e.AuthRequired = len(denied[id]) > 0 && !anonOK[id]
		endpoints[id] = e
	}
	for _, o := range valid {
		ep := endpoints[o.Endpoint.ID]
		refs := evidence(o)
		addEdge(Edge{From: o.Identity.ID, To: ep.ID, Kind: "ACCESSES", Evidence: refs, Status: o.Status})
		if o.Status >= 200 && o.Status < 300 {
			identity := identities[o.Identity.ID]
			identity.Capabilities = union(identity.Capabilities, []string{ep.ID})
			identities[o.Identity.ID] = identity
		}
		if o.Identity.TokenRef != "anon" && o.Status == 200 && len(denied[ep.ID]) > 0 {
			addEdge(Edge{From: o.Identity.ID, To: ep.ID, Kind: "UNLOCKS", Evidence: union(refs, denied[ep.ID]), Status: 200})
		}
		for _, info := range o.Infos {
			infos[info.ID] = info
			addEdge(Edge{From: ep.ID, To: info.ID, Kind: "EXPOSES", Evidence: refs})
		}
		for _, s := range o.Sinks {
			if old, ok := sinks[s.ID]; !ok || s.Confidence > old.Confidence {
				sinks[s.ID] = s
			}
		}
		for _, p := range o.Parameters {
			if old, ok := params[p.ID]; ok {
				p.ObservedTypes = union(p.ObservedTypes, old.ObservedTypes)
				p.Samples = union(p.Samples, old.Samples)
				p.ValueDigests = union(p.ValueDigests, old.ValueDigests)
				p.Reflected = p.Reflected || old.Reflected
				if p.ReflectionContext == "" {
					p.ReflectionContext = old.ReflectionContext
				}
			}
			sort.Strings(p.Samples)
			if len(p.Samples) > 5 {
				p.Samples = p.Samples[:5]
			}
			p.DistinctCount = len(p.ValueDigests)
			params[p.ID] = p
			addEdge(Edge{From: ep.ID, To: p.ID, Kind: "HAS_PARAM", Evidence: refs})
		}
	}
	for _, o := range valid {
		ep := endpoints[o.Endpoint.ID]
		refs := evidence(o)
		xssAllowed := allowsXSS(o)
		for _, p := range o.Parameters {
			add := func(class string, s Sink, reason string) {
				why := append([]string{}, refs...)
				why = append(why, reason, fmt.Sprintf("reflected:%t", p.Reflected), "hypothesis:not_confirmed")
				identityBonus := 1.0
				if len(identities) >= 2 && (class == "idor" || class == "bfla" || class == "bopla") {
					identityBonus = 1.5
					why = append(why, "identity:multiple_observed")
					for _, other := range valid {
						if other.Identity.ID != o.Identity.ID {
							why = union(why, evidence(other))
							break
						}
					}
				}
				id := node("cand", ep.ID+"|"+p.ID+"|"+class)
				c := Candidate{ID: id, Class: class, Endpoint: ep.Method + " " + ep.Route, EndpointID: ep.ID, Parameter: p.Name, Location: p.Location, Sink: s, Why: union(why, nil), Auth: Auth{Required: ep.AuthRequired}, Novelty: "untested", Playbook: "playbooks/" + strings.ReplaceAll(class, "/", "-") + ".md", SuggestedCheck: "Use controlled differential probes for " + class + " at " + p.Location + " parameter " + p.Name + "; compare baseline and identity controls. No vulnerability is confirmed."}
				c.Score = score(s.Confidence, p.Reflected, ClassFit(p.Name, class), identityBonus, c.Novelty, ep.StateChanging, class)
				if old, ok := candidates[id]; ok {
					c.Why = union(old.Why, c.Why)
					if old.Score > c.Score {
						old.Why = c.Why
						c = old
					}
				}
				candidates[id] = c
				if old, ok := sinks[s.ID]; !ok || old.Confidence < s.Confidence {
					sinks[s.ID] = s
				}
				if s.Kind != "input_control" && s.Kind != "authorization" {
					addEdge(Edge{CandidateID: id, From: p.ID, To: s.ID, Kind: "REACHES", Evidence: refs})
				}
			}
			for _, s := range o.Sinks {
				class := map[string]string{"sql": "sqli", "template": "ssti", "eval": "eval/js", "fs": "path-traversal", "deser": "deser"}[s.Kind]
				if class != "" {
					add(class, s, "error_signature:"+s.Signature)
					id := node("cand", ep.ID+"|"+p.ID+"|"+class)
					c := candidates[id]
					c.Why = union(c.Why, []string{"co_observed:not_taint_proof"})
					candidates[id] = c
				}
			}
			if p.Reflected {
				class := "xss"
				if p.ReflectionContext == "json" {
					class = "input-validation"
				}
				if class != "xss" || xssAllowed {
					add(class, synthetic("reflection", p.ReflectionContext, "reflected:"+p.ReflectionContext, 0.6, ep.ID), "reflection_context:"+p.ReflectionContext)
				}
			}
			if secretName.MatchString(p.Name) {
				continue
			}
			for _, class := range Classes {
				if class == "xss" && !xssAllowed {
					continue
				}
				if ClassFit(p.Name, class) != 1 {
					continue
				}
				if (class == "bfla" || class == "bopla") && len(identities) < 2 {
					continue
				}
				kind := "input_control"
				if class == "bfla" || class == "bopla" || class == "idor" {
					kind = "authorization"
				}
				// Name-fit candidates remain explicit input-control hypotheses. They do
				// not masquerade as SQL, SSRF or filesystem execution evidence.
				add(class, synthetic(kind, "unknown", "parameter_observed", 0.3, ep.ID), "parameter_observed:"+p.Location+":"+p.Name)
			}
		}
	}
	for _, k := range sortedKeys(endpoints) {
		g.Endpoints = append(g.Endpoints, endpoints[k])
	}
	for _, k := range sortedKeys(params) {
		g.Parameters = append(g.Parameters, params[k])
	}
	for _, k := range sortedKeys(sinks) {
		g.Sinks = append(g.Sinks, sinks[k])
	}
	for _, k := range sortedKeys(identities) {
		g.Identities = append(g.Identities, identities[k])
	}
	for _, k := range sortedKeys(infos) {
		g.Infos = append(g.Infos, infos[k])
	}
	for _, k := range sortedKeys(edges) {
		g.Edges = append(g.Edges, edges[k])
	}
	for _, k := range sortedKeys(candidates) {
		g.Result.Candidates = append(g.Result.Candidates, candidates[k])
	}
	sort.Slice(g.Result.Candidates, func(i, j int) bool {
		a, b := g.Result.Candidates[i], g.Result.Candidates[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.ID < b.ID
	})
	g.Result.Stats = Stats{Endpoints: len(g.Endpoints), Parameters: len(g.Parameters), Sinks: len(g.Sinks), Identities: len(g.Identities), Infos: len(g.Infos), Observations: len(valid), Candidates: len(candidates)}
	return g
}
func (g Graph) Query(classes []string, limit int) (Result, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 1000 {
		return Result{}, errors.New("attack-surface limit must be between 1 and 1000")
	}
	allowed := map[string]bool{}
	for _, c := range classes {
		valid := false
		for _, known := range Classes {
			if c == known {
				valid = true
			}
		}
		if !valid {
			return Result{}, fmt.Errorf("unsupported attack class %q", c)
		}
		allowed[c] = true
	}
	r := g.Result
	r.Candidates = []Candidate{}
	r.Uncovered = []Gap{}
	present := map[string]bool{}
	for _, c := range g.Result.Candidates {
		if len(allowed) == 0 || allowed[c.Class] {
			present[c.Class] = true
			r.Candidates = append(r.Candidates, c)
		}
	}
	requested := classes
	if len(requested) == 0 {
		requested = Classes
	}
	for _, c := range requested {
		if !present[c] {
			r.Uncovered = append(r.Uncovered, Gap{Class: c, Reason: "no evidence-backed input/sink candidate in imported captures"})
		}
	}
	if len(r.Candidates) > limit {
		r.Candidates = r.Candidates[:limit]
	}
	return r, nil
}
