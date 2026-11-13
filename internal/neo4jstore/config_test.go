package neo4jstore

import (
	"strings"
	"testing"
)

func TestConfigFromRequiredEnvPrefixDoesNotFallBackToGraphDefaults(t *testing.T) {
	t.Setenv("BB_ASSISTANT_NEO4J_URI", "bolt://127.0.0.1:7687")
	t.Setenv("BB_ASSISTANT_NEO4J_USERNAME", "graph-user")
	t.Setenv("BB_ASSISTANT_NEO4J_PASSWORD", "graph-secret")
	t.Setenv("BB_ASSISTANT_NEO4J_DATABASE", "neo4j")
	for _, suffix := range []string{"URI", "USERNAME", "PASSWORD", "DATABASE"} {
		t.Setenv("BB_ASSISTANT_KB_NEO4J_"+suffix, "")
	}

	if _, err := ConfigFromRequiredEnvPrefix("BB_ASSISTANT_KB_NEO4J"); err == nil {
		t.Fatal("required KB Neo4j config unexpectedly reused the graph config")
	} else if strings.Contains(err.Error(), "graph-secret") {
		t.Fatal("configuration error exposed a credential")
	}
}

func TestConfigFromRequiredEnvPrefixReadsDedicatedEndpoint(t *testing.T) {
	t.Setenv("BB_ASSISTANT_KB_NEO4J_URI", "bolt+s://kb.example.internal:7687")
	t.Setenv("BB_ASSISTANT_KB_NEO4J_USERNAME", "kb-user")
	t.Setenv("BB_ASSISTANT_KB_NEO4J_PASSWORD", "kb-secret")
	t.Setenv("BB_ASSISTANT_KB_NEO4J_DATABASE", "knowledge")

	cfg, err := ConfigFromRequiredEnvPrefix("BB_ASSISTANT_KB_NEO4J")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URI != "bolt+s://kb.example.internal:7687" || cfg.Username != "kb-user" || cfg.Password != "kb-secret" || cfg.Database != "knowledge" {
		t.Fatal("dedicated endpoint fields were not loaded as configured")
	}
}

func TestConfigFromRequiredEnvPrefixRejectsUntrustedPrefix(t *testing.T) {
	if _, err := ConfigFromRequiredEnvPrefix("BB_ASSISTANT_NEO4J_PASSWORD=secret"); err == nil {
		t.Fatal("environment prefix injection was accepted")
	}
}
