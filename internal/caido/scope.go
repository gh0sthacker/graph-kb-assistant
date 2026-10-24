package caido

import (
	"net"
	"strings"
)

// NormalizeRequestHost validates a host that will be embedded in a closed-set
// Caido HTTPQL equality filter. Wildcards, ports, paths and query syntax are
// deliberately rejected.
func NormalizeRequestHost(host string) (string, error) {
	if host == "" || host != strings.TrimSpace(host) || len(host) > 253 {
		return "", sanitized("Caido scope host is invalid")
	}
	host = strings.ToLower(host)
	if address := net.ParseIP(host); address != nil {
		return address.String(), nil
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", sanitized("Caido scope host is invalid")
		}
		for _, char := range label {
			if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-') {
				return "", sanitized("Caido scope host is invalid")
			}
		}
	}
	return host, nil
}

// RequestHostHTTPQLFilter returns the exact-host equality filter used by the
// no-switch history inventory path.
func RequestHostHTTPQLFilter(host string) (string, error) {
	normalized, err := NormalizeRequestHost(host)
	if err != nil {
		return "", err
	}
	return `req.host.eq:"` + normalized + `"`, nil
}
