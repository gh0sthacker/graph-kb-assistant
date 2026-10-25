package graph

import (
	"errors"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"bb-graph-assistant/internal/caido"
)

type pageNavigationItem struct {
	bundle        Record
	observationID string
	requestURL    string
	refererURL    string
	redirectURL   string
}

// AddPageNavigationAssertions adds exact, source-backed HTTP navigation links
// between exchanges in one imported page. It intentionally does not infer
// links from timestamps, route templates, or graph proximity.
func AddPageNavigationAssertions(bundles []Record, exchanges []caido.HttpExchange) error {
	if len(bundles) == 0 && len(exchanges) == 0 {
		return nil
	}
	if len(bundles) != len(exchanges) || len(bundles) > 100 {
		return errors.New("navigation page does not match its graph bundles")
	}
	items := make([]pageNavigationItem, len(bundles))
	requestIDs := make(map[string][]int, len(bundles))
	redirectIDs := make(map[string][]int, len(bundles))
	for index, bundle := range bundles {
		projectKey, projectErr := exchanges[index].Ref.Project.StorageKey()
		bundleProject, projectOK := bundle["program"].(string)
		entities, ok := bundle["entities"].([]Record)
		if projectErr != nil || !projectOK || bundleProject != projectKey || !ok {
			return errors.New("navigation bundle namespace or entities are malformed")
		}
		observationID := ""
		for _, entity := range entities {
			if entity["kind"] == "observation" {
				observationID, _ = entity["id"].(string)
				break
			}
		}
		requestURL, okURL := exchangeNavigationURL(exchanges[index])
		if !ok || !okURL || !strings.HasPrefix(observationID, "observation-") {
			return errors.New("navigation bundle observation or request URL is invalid")
		}
		current := pageNavigationItem{bundle: bundle, observationID: observationID, requestURL: requestURL}
		if value, ok := uniqueHTTPHeader(exchanges[index].RequestRaw, "referer"); ok {
			current.refererURL, _ = resolveNavigationURL(value, exchanges[index])
		}
		if responseStatusIsRedirect(exchanges[index]) {
			if value, ok := uniqueHTTPHeader(exchanges[index].ResponseRaw, "location"); ok {
				current.redirectURL, _ = resolveNavigationURL(value, exchanges[index])
			}
		}
		items[index] = current
		requestIDs[requestURL] = append(requestIDs[requestURL], index)
		if current.redirectURL != "" {
			redirectIDs[current.redirectURL] = append(redirectIDs[current.redirectURL], index)
		}
	}
	for index, current := range items {
		if current.refererURL != "" {
			if candidates := requestIDs[current.refererURL]; len(candidates) == 1 && candidates[0] != index {
				if err := appendNavigationAssertion(items[candidates[0]], current, "referrer_context_for", "referrer_exact_url"); err != nil {
					return err
				}
			}
		}
		if candidates := redirectIDs[current.requestURL]; len(candidates) == 1 && candidates[0] != index {
			if err := appendNavigationAssertion(items[candidates[0]], current, "redirects_to", "redirect_exact_url"); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendNavigationAssertion(predecessor, current pageNavigationItem, predicate, basis string) error {
	predecessorRefs, okPrevious := observationSourceRefs(predecessor.bundle, predecessor.observationID)
	currentRefs, okCurrent := observationSourceRefs(current.bundle, current.observationID)
	if !okPrevious || !okCurrent || len(predecessorRefs) == 0 || len(currentRefs) == 0 {
		return errors.New("navigation assertion source receipts are unavailable")
	}
	predecessorEntities, okPreviousEntities := predecessor.bundle["entities"].([]Record)
	currentEntities, okCurrentEntities := current.bundle["entities"].([]Record)
	if !okPreviousEntities || !okCurrentEntities {
		return errors.New("navigation assertion observations are unavailable")
	}
	var predecessorEntity Record
	for _, entity := range predecessorEntities {
		if entity["id"] == predecessor.observationID && entity["kind"] == "observation" {
			predecessorEntity = entity
			break
		}
	}
	if predecessorEntity == nil {
		return errors.New("navigation predecessor observation is unavailable")
	}
	entityPresent := false
	for _, entity := range currentEntities {
		if entity["id"] == predecessor.observationID {
			entityPresent = true
			break
		}
	}
	if !entityPresent {
		currentEntities = append(currentEntities, predecessorEntity)
		sort.Slice(currentEntities, func(i, j int) bool {
			return currentEntities[i]["id"].(string) < currentEntities[j]["id"].(string)
		})
		current.bundle["entities"] = currentEntities
	}
	refs := append(append([]Record(nil), predecessorRefs...), currentRefs...)
	seen := make(map[string]struct{}, len(refs))
	uniqueRefs := refs[:0]
	for _, ref := range refs {
		sha, _ := ref["sha256"].(string)
		locator, _ := ref["record_locator"].(string)
		key := sha + "\x00" + locator
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		uniqueRefs = append(uniqueRefs, ref)
	}
	sort.Slice(uniqueRefs, func(i, j int) bool {
		leftSHA, _ := uniqueRefs[i]["sha256"].(string)
		rightSHA, _ := uniqueRefs[j]["sha256"].(string)
		if leftSHA != rightSHA {
			return leftSHA < rightSHA
		}
		leftLocator, _ := uniqueRefs[i]["record_locator"].(string)
		rightLocator, _ := uniqueRefs[j]["record_locator"].(string)
		return leftLocator < rightLocator
	})
	current.bundle["sources"] = uniqueRefs
	program, _ := current.bundle["program"].(string)
	context := Record{
		"program": program, "link_basis": basis,
		"causality": "navigation_evidence_only",
	}
	assertion, err := historicalAssertion(program, predecessor.observationID, predicate, current.observationID, context, uniqueRefs)
	if err != nil {
		return errors.New("navigation assertion identity could not be computed")
	}
	assertions, ok := current.bundle["assertions"].([]Record)
	if !ok {
		return errors.New("navigation bundle assertions are malformed")
	}
	for _, existing := range assertions {
		if existing["id"] == assertion["id"] {
			return nil
		}
	}
	assertions = append(assertions, assertion)
	sort.Slice(assertions, func(i, j int) bool {
		return assertions[i]["id"].(string) < assertions[j]["id"].(string)
	})
	current.bundle["assertions"] = assertions
	return nil
}

func observationSourceRefs(bundle Record, observationID string) ([]Record, bool) {
	entities, ok := bundle["entities"].([]Record)
	if !ok {
		return nil, false
	}
	for _, entity := range entities {
		if entity["kind"] != "observation" || entity["id"] != observationID {
			continue
		}
		refs, ok := entity["source_refs"].([]Record)
		return refs, ok && len(refs) > 0
	}
	return nil, false
}

func uniqueHTTPHeader(raw []byte, name string) (string, bool) {
	headers, _, boundary, complete := splitMessage(raw)
	if !boundary || !complete {
		return "", false
	}
	value := ""
	found := false
	for _, header := range headers {
		if header.name != name {
			continue
		}
		if found {
			return "", false
		}
		value = strings.TrimSpace(header.value)
		found = value != ""
	}
	return value, found
}

func responseStatusIsRedirect(exchange caido.HttpExchange) bool {
	if !exchange.HasResponse || exchange.ResponseStatus == nil {
		return false
	}
	status := *exchange.ResponseStatus
	return status >= 300 && status < 400
}

func exchangeNavigationURL(exchange caido.HttpExchange) (string, bool) {
	_, target := requestLine(exchange.RequestRaw)
	target = firstNavigationValue(exchange.Target, target)
	return resolveNavigationURL(target, exchange)
}

func resolveNavigationURL(target string, exchange caido.HttpExchange) (string, bool) {
	if target == "" || len(target) > 65_536 {
		return "", false
	}
	parsed, err := url.Parse(strings.TrimSpace(target))
	if err != nil || parsed.User != nil {
		return "", false
	}
	if !parsed.IsAbs() {
		scheme := strings.ToLower(exchange.Scheme)
		if scheme != "http" && scheme != "https" {
			return "", false
		}
		host, err := caido.NormalizeRequestHost(exchange.Host)
		if err != nil || exchange.Port < 1 || exchange.Port > 65535 {
			return "", false
		}
		authority := host
		if strings.Contains(host, ":") {
			authority = "[" + host + "]"
		}
		defaultPort := 80
		if scheme == "https" {
			defaultPort = 443
		}
		if exchange.Port != defaultPort {
			authority = net.JoinHostPort(host, strconv.Itoa(exchange.Port))
		}
		base := &url.URL{Scheme: scheme, Host: authority, Path: "/"}
		parsed = base.ResolveReference(parsed)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
		return "", false
	}
	host, err := caido.NormalizeRequestHost(parsed.Hostname())
	if err != nil {
		return "", false
	}
	port := parsed.Port()
	if port != "" {
		parsedPort, parseErr := strconv.Atoi(port)
		if parseErr != nil || parsedPort < 1 || parsedPort > 65535 {
			return "", false
		}
	}
	defaultPort := "80"
	if parsed.Scheme == "https" {
		defaultPort = "443"
	}
	if port == defaultPort || port == "" {
		parsed.Host = host
		if strings.Contains(host, ":") {
			parsed.Host = "[" + host + "]"
		}
	} else {
		parsed.Host = net.JoinHostPort(host, port)
	}
	parsed.Fragment = ""
	parsed.RawFragment = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.String(), true
}

func firstNavigationValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
