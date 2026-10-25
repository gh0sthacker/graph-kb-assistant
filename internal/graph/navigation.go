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
		if !ok || !strings.HasPrefix(observationID, "observation-") {
			return errors.New("navigation bundle observation is invalid")
		}
		current := pageNavigationItem{bundle: bundle, observationID: observationID, requestURL: requestURL}
		if okURL {
			if value, ok := uniqueHTTPHeader(exchanges[index].RequestRaw, "referer"); ok {
				current.refererURL, _ = resolveNavigationURL(value, exchanges[index])
			}
		}
		if okURL && responseStatusIsRedirect(exchanges[index]) {
			if value, ok := uniqueHTTPHeader(exchanges[index].ResponseRaw, "location"); ok {
				current.redirectURL, _ = resolveNavigationURL(value, exchanges[index])
			}
		}
		items[index] = current
		if okURL {
			requestIDs[requestURL] = append(requestIDs[requestURL], index)
		}
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

// NavigationURLIndexKeys returns project-keyed URL identities for a request,
// its unique Referer, and its unique redirect Location. Invalid or ambiguous
// navigation metadata produces an empty optional key instead of raw data.
func NavigationURLIndexKeys(exchange caido.HttpExchange, key []byte) (string, string, string, error) {
	if len(key) != 32 {
		return "", "", "", errors.New("navigation index key is invalid")
	}
	requestURL, ok := exchangeNavigationURL(exchange)
	if !ok {
		return "", "", "", nil
	}
	requestKey, err := URLIndexKey(requestURL, "", key)
	if err != nil {
		return "", "", "", errors.New("navigation request URL key could not be computed")
	}
	var refererKey, redirectKey string
	if value, exists := uniqueHTTPHeader(exchange.RequestRaw, "referer"); exists {
		if resolved, valid := resolveNavigationURL(value, exchange); valid {
			refererKey, err = URLIndexKey(resolved, "", key)
			if err != nil {
				return "", "", "", errors.New("navigation Referer URL key could not be computed")
			}
		}
	}
	if responseStatusIsRedirect(exchange) {
		if value, exists := uniqueHTTPHeader(exchange.ResponseRaw, "location"); exists {
			if resolved, valid := resolveNavigationURL(value, exchange); valid {
				redirectKey, err = URLIndexKey(resolved, "", key)
				if err != nil {
					return "", "", "", errors.New("navigation redirect URL key could not be computed")
				}
			}
		}
	}
	return requestKey, refererKey, redirectKey, nil
}

func appendNavigationAssertion(predecessor, current pageNavigationItem, predicate, basis string) error {
	return AddNavigationEvidenceAssertion(
		current.bundle, predecessor.bundle, predecessor.observationID,
		current.bundle, current.observationID, predicate, basis,
	)
}

// AddNavigationEvidenceAssertion attaches an exact, source-backed navigation
// assertion to ownerBundle. Subject and object may come from different
// persisted capture bundles; their observation records and receipt references
// are copied into the owning bundle for idempotent asynchronous projection.
func AddNavigationEvidenceAssertion(
	ownerBundle, subjectBundle Record, subjectID string, objectBundle Record, objectID string,
	predicate, basis string,
) error {
	if predicate != "referrer_context_for" && predicate != "redirects_to" ||
		(predicate == "referrer_context_for" && basis != "referrer_exact_url") ||
		(predicate == "redirects_to" && basis != "redirect_exact_url") {
		return errors.New("navigation assertion basis is invalid")
	}
	program, ownerOK := ownerBundle["program"].(string)
	subjectProgram, subjectOK := subjectBundle["program"].(string)
	objectProgram, objectOK := objectBundle["program"].(string)
	if !ownerOK || !subjectOK || !objectOK || program == "" || program != subjectProgram || program != objectProgram {
		return errors.New("navigation assertion project namespaces do not match")
	}
	subjectRefs, okSubject := observationSourceRefs(subjectBundle, subjectID)
	objectRefs, okObject := observationSourceRefs(objectBundle, objectID)
	if !okSubject || !okObject || len(subjectRefs) == 0 || len(objectRefs) == 0 {
		return errors.New("navigation assertion source receipts are unavailable")
	}
	ownerEntities, okOwnerEntities := ownerBundle["entities"].([]Record)
	subjectEntities, okSubjectEntities := subjectBundle["entities"].([]Record)
	objectEntities, okObjectEntities := objectBundle["entities"].([]Record)
	if !okOwnerEntities || !okSubjectEntities || !okObjectEntities {
		return errors.New("navigation assertion observations are unavailable")
	}
	assertionRefs := navigationSourceRefs(append(append([]Record(nil), subjectRefs...), objectRefs...))
	bundleRefs := append([]Record(nil), assertionRefs...)
	if ownerRefs, ok := ownerBundle["sources"].([]Record); ok {
		bundleRefs = append(bundleRefs, ownerRefs...)
	}
	ownerEntities = appendNavigationEntity(ownerEntities, subjectEntities, subjectID)
	ownerEntities = appendNavigationEntity(ownerEntities, objectEntities, objectID)
	if !containsNavigationEntity(ownerEntities, subjectID) || !containsNavigationEntity(ownerEntities, objectID) {
		return errors.New("navigation assertion endpoint observations are unavailable")
	}
	sort.Slice(ownerEntities, func(i, j int) bool {
		return ownerEntities[i]["id"].(string) < ownerEntities[j]["id"].(string)
	})
	ownerBundle["entities"] = ownerEntities
	ownerBundle["sources"] = navigationSourceRefs(bundleRefs)
	context := Record{
		"program": program, "link_basis": basis,
		"causality": "navigation_evidence_only",
	}
	assertion, err := historicalAssertion(program, subjectID, predicate, objectID, context, assertionRefs)
	if err != nil {
		return errors.New("navigation assertion identity could not be computed")
	}
	assertions, ok := ownerBundle["assertions"].([]Record)
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
	ownerBundle["assertions"] = assertions
	return nil
}

func navigationSourceRefs(refs []Record) []Record {
	seen := make(map[string]struct{}, len(refs))
	unique := make([]Record, 0, len(refs))
	for _, ref := range refs {
		key := sourcePair(ref)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, ref)
	}
	sort.Slice(unique, func(i, j int) bool {
		leftSHA, _ := unique[i]["sha256"].(string)
		rightSHA, _ := unique[j]["sha256"].(string)
		if leftSHA != rightSHA {
			return leftSHA < rightSHA
		}
		leftLocator, _ := unique[i]["record_locator"].(string)
		rightLocator, _ := unique[j]["record_locator"].(string)
		return leftLocator < rightLocator
	})
	return unique
}

func appendNavigationEntity(owner, source []Record, observationID string) []Record {
	for _, entity := range owner {
		if entity["id"] == observationID {
			return owner
		}
	}
	for _, entity := range source {
		if entity["kind"] == "observation" && entity["id"] == observationID {
			return append(owner, entity)
		}
	}
	return owner
}

func containsNavigationEntity(entities []Record, observationID string) bool {
	for _, entity := range entities {
		if entity["kind"] == "observation" && entity["id"] == observationID {
			return true
		}
	}
	return false
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
