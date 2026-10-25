package publicdata

import (
	"strings"
	"testing"
)

func TestValidateJSONRejectsPrivateAndSecretLikeGraphData(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"private request field", `{"raw_request":"GET /private HTTP/1.1"}`},
		{"secret assignment", `{"description":"password=secret-value"}`},
		{"prefixed secret assignment", `{"description":"client_secret=opaque-value"}`},
		{"session assignment", `{"description":"session=opaque-value"}`},
		{"secret key assignment", `{"description":"secret_key=opaque-value"}`},
		{"bearer credential", `{"description":"Authorization: Bearer abcdefghijklmnop"}`},
		{"unsafe url query", `{"url":"https://example.test/path?user=alice"}`},
		{"duplicate object key", `{"status":"safe","status":"raw"}`},
		{"trailing value", `{"status":"safe"} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateJSON([]byte(test.input)); err == nil {
				t.Fatal("expected private or ambiguous metadata to be rejected")
			}
		})
	}
}

func TestSecretLikeScanFastPathPreservesLongSafeText(t *testing.T) {
	value := strings.Repeat("ordinary-route-segment-", 40_000)
	if containsSecretLikeValue(value) {
		t.Fatal("long ordinary text was classified as secret-like")
	}
	for _, value := range []string{
		`PassWord="opaque-value"`,
		`client_api-key=opaque-value`,
		`{"user_email":"person@example.test"}`,
		`Authorization: Bearer abcdefghijklmnop`,
		`-----BEGIN PRIVATE KEY-----`,
	} {
		if !containsSecretLikeValue(value) {
			t.Errorf("sensitive marker was not detected in %q", value)
		}
	}
}

func TestValidateAllowsRedactedValuesAndShapeOnlyUrls(t *testing.T) {
	input := []byte(`{"token":"[REDACTED]","url":"https://example.test/path?token=%5BREDACTED%5D","fields":[{"name":"id","value":""}]}`)
	if err := ValidateJSON(input); err != nil {
		t.Fatalf("valid redacted metadata was rejected: %v", err)
	}
}

func TestRedactTextRemovesSensitiveValuesAndReportsOriginalByteSpans(t *testing.T) {
	input := "GET /items?access_token=query-secret HTTP/1.1\r\n" +
		"Authorization: Basic dXNlcjpzZWNyZXQ=\r\nCookie: session=header-secret\r\n\r\n" +
		`{"password":"body-secret","session":"session-secret","secret_key":"key-secret","email":"max@example.test","id":"42"}`
	redacted, spans, err := RedactText(input)
	if err != nil {
		t.Fatalf("RedactText() error = %v", err)
	}
	for _, secret := range []string{"query-secret", "dXNlcjpzZWNyZXQ=", "header-secret", "body-secret", "session-secret", "key-secret", "max@example.test"} {
		if strings.Contains(redacted, secret) {
			t.Errorf("redacted text still contains %q: %s", secret, redacted)
		}
	}
	if len(spans) < 7 {
		t.Fatalf("redaction spans = %#v, want header, assignment, and personal-data coverage", spans)
	}
	for _, span := range spans {
		if span.Start < 0 || span.End > len(input) || span.End <= span.Start || span.Rule == "" {
			t.Errorf("redaction span is invalid: %#v", span)
		}
	}
	if !strings.Contains(redacted, `"password":"[REDACTED]"`) || !strings.Contains(redacted, `"id":"42"`) {
		t.Fatalf("redaction did not preserve safe JSON structure: %s", redacted)
	}
	if err := Validate(map[string]string{"excerpt": redacted}); err != nil {
		t.Fatalf("redacted excerpt failed the public-data gate: %v", err)
	}
}

func TestRedactTextRemovesCompletePrivateKeyBlockAndRejectsBinaryText(t *testing.T) {
	input := "begin\n-----BEGIN PRIVATE KEY-----\nsecret-private-material\n-----END PRIVATE KEY-----\nend"
	redacted, spans, err := RedactText(input)
	if err != nil || strings.Contains(redacted, "secret-private-material") || len(spans) != 1 || spans[0].Rule != "private-key-block+secret-like-value" {
		t.Fatalf("private key redaction = %q, %#v, %v", redacted, spans, err)
	}
	if _, _, err := RedactText("binary\xff"); err == nil {
		t.Fatal("RedactText accepted invalid UTF-8")
	}
}

func TestRedactTextRemovesCompleteQuotedCredentialsAndPIIAssignments(t *testing.T) {
	input := "password=\"my secret phrase\"\n" +
		`{"client_secret":"two words with \"quote\" inside","phone":"+1 555 0100","user_email":"max@example.test","safe":"keep me"}`
	redacted, spans, err := RedactText(input)
	if err != nil {
		t.Fatalf("RedactText() error = %v", err)
	}
	for _, sensitive := range []string{"my secret phrase", "two words", "quote", "+1 555 0100", "max@example.test"} {
		if strings.Contains(redacted, sensitive) {
			t.Errorf("redacted text still contains %q: %s", sensitive, redacted)
		}
	}
	if !strings.Contains(redacted, `password="[REDACTED]"`) || !strings.Contains(redacted, `"client_secret":"[REDACTED]"`) || !strings.Contains(redacted, `"phone":"[REDACTED]"`) || !strings.Contains(redacted, `"safe":"keep me"`) {
		t.Fatalf("redaction did not preserve JSON structure and safe fields: %s", redacted)
	}
	if len(spans) != 4 {
		t.Fatalf("redaction spans = %#v, want one span per sensitive assignment", spans)
	}
	if err := Validate(map[string]string{"excerpt": redacted}); err != nil {
		t.Fatalf("fully redacted text failed the public-data gate: %v", err)
	}
}
