package appquery

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
)

func TestGraphContinuationFailureLogsInternalCause(t *testing.T) {
	oldOutput, oldFlags := log.Writer(), log.Flags()
	var stderr bytes.Buffer
	log.SetOutput(&stderr)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
	})
	t.Setenv(graphContinuationDiagnosticsEnv, "")

	message := graphContinuationFailureMessage(errors.New("continuation store read failed"))
	if message != "full graph continuation page could not be read" {
		t.Fatalf("default continuation error = %q", message)
	}
	if !strings.Contains(stderr.String(), "continuation store read failed") {
		t.Fatalf("stderr did not include internal cause: %q", stderr.String())
	}
}

func TestGraphContinuationFailureResponseCanExposeOnlyRedactedCause(t *testing.T) {
	oldOutput, oldFlags := log.Writer(), log.Flags()
	var stderr bytes.Buffer
	log.SetOutput(&stderr)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
	})
	t.Setenv(graphContinuationDiagnosticsEnv, "1")

	cause := errors.New("Neo4j query failed password=do-not-show authorization: Bearer bearer-secret-value bolt://user:uri-secret@db:7687 neo4j+ssc://driver-user:driver-secret@secure-db:7687 request body: raw-request-body")
	message := graphContinuationFailureMessage(cause)
	if !strings.Contains(message, "internal error:") || !strings.Contains(message, "Neo4j query failed") {
		t.Fatalf("diagnostic response omitted the internal cause: %q", message)
	}
	if strings.Count(message, "[REDACTED URI]") != 2 {
		t.Fatalf("expected both connection URIs to be fully redacted: %q", message)
	}
	for _, secret := range []string{"do-not-show", "also-secret", "bearer-secret-value", "uri-secret", "driver-user", "driver-secret", "secure-db:7687", "bolt://", "neo4j+ssc://", "raw-request-body"} {
		if strings.Contains(message, secret) || strings.Contains(stderr.String(), secret) {
			t.Fatalf("diagnostic leaked %q: response=%q stderr=%q", secret, message, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "Neo4j query failed") {
		t.Fatalf("stderr omitted the internal cause: %q", stderr.String())
	}
}
