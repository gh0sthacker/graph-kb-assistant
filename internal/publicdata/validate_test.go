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
		{"session assignment", `{"description":"session=8f14e45fceea167a5a36dedd4bea2543"}`},
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

func TestRedactTextPreservesProseWithSensitiveWords(t *testing.T) {
	for _, input := range []string{
		"QA test session: verify consultation review readback; no model submission.",
		"the token: rotate later",
		"email me when ready",
		"session: review_later",
		`{"session":"review later"}`,
		`{"session":"review_later"}`,
	} {
		t.Run(input, func(t *testing.T) {
			redacted, spans, err := RedactText(input)
			if err != nil {
				t.Fatalf("RedactText() error = %v", err)
			}
			if redacted != input || len(spans) != 0 {
				t.Fatalf("prose changed: got %q with spans %#v", redacted, spans)
			}
			if err := Validate(map[string]string{"excerpt": redacted}); err != nil {
				t.Fatalf("ordinary prose failed validation: %v", err)
			}
		})
	}
}

func TestRedactTextPreservesDetectionOfCredentialValues(t *testing.T) {
	privateKey := "-----BEGIN PRIVATE KEY-----\\nsecret-private-material\\n-----END PRIVATE KEY-----"
	for _, test := range []struct {
		name   string
		input  string
		secret string
	}{
		{"opaque session id", "session=8f14e45fceea167a5a36dedd4bea2543", "8f14e45fceea167a5a36dedd4bea2543"},
		{"bearer JWT", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abc", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abc"},
		{"JSON authorization", `{"authorization":"Basic dXNlcjpzZWNyZXQ="}`, "dXNlcjpzZWNyZXQ="},
		{"JSON access token", `{"access_token":"ya29.a0AfH6"}`, "ya29.a0AfH6"},
		{"password assignment", "password: hunter2", "hunter2"},
		{"PEM private key", privateKey, "secret-private-material"},
		{"cookie header", "Cookie: sid=abc123", "sid=abc123"},
		{"Set-Cookie header", "Set-Cookie: sid=abc123", "sid=abc123"},
		{"api key assignment", "api_key=ya29.a0AfH6", "ya29.a0AfH6"},
		{"composite session id", "session_id=verify", "verify"},
		{"composite session cookie", "session_cookie=verify", "verify"},
		{"private key assignment", "private_key=short", "short"},
		{"credential assignment", "credential=short", "short"},
		{"refresh token assignment", "refresh_token=short", "short"},
		{"CSRF token assignment", "csrf_token=short", "short"},
		{"passwd assignment", "passwd=short", "short"},
	} {
		t.Run(test.name, func(t *testing.T) {
			redacted, _, err := RedactText(test.input)
			if err != nil {
				t.Fatalf("RedactText() error = %v", err)
			}
			if strings.Contains(redacted, test.secret) {
				t.Fatalf("secret %q survived redaction: %s", test.secret, redacted)
			}
			if err := Validate(map[string]string{"excerpt": redacted}); err != nil {
				t.Fatalf("redacted secret failed validation: %v", err)
			}
			if err := Validate(map[string]string{"excerpt": test.input}); err == nil {
				t.Fatalf("secret-bearing input passed validation: %q", test.input)
			}
		})
	}
}

func TestRedactTextRemovesSensitiveValuesAndReportsOriginalByteSpans(t *testing.T) {
	input := "GET /items?access_token=query-secret HTTP/1.1\r\n" +
		"Authorization: Basic dXNlcjpzZWNyZXQ=\r\nCookie: session=header-secret\r\n\r\n" +
		`{"password":"body-secret","session":"8f14e45fceea167a5a36dedd4bea2543","secret_key":"key-secret","email":"max@example.test","id":"42"}`
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

func TestBareSensitiveKeysMaskShortOpaqueValues(t *testing.T) {
	for _, test := range []struct {
		name   string
		input  string
		secret string
	}{
		{"short opaque session", `{"session":"a1b2c3"}`, "a1b2c3"},
		{"short opaque token", `{"token":"x9"}`, "x9"},
		{"short opaque cookie", `{"cookie":"abc123"}`, "abc123"},
		{"short opaque jwt", `{"jwt":"q7"}`, "q7"},
		{"short opaque secret", `{"secret":"s3cr3t"}`, "s3cr3t"},
		{"bearer authorization", `{"authorization":"Bearer abc"}`, "Bearer abc"},
		{"long bare-key value without digits", `{"session":"abcdefghijkl"}`, "abcdefghijkl"},
		{"value outside the word alphabet", `{"token":"abc/def"}`, "abc/def"},
		{"composite session id", `{"session_id":"verify"}`, "verify"},
		{"composite session cookie", `{"session_cookie":"verify"}`, "verify"},
		{"composite api key", `{"api_key":"short"}`, "short"},
		{"composite csrf token", `{"csrf_token":"short"}`, "short"},
		{"password field", `{"password":"short"}`, "short"},
		{"private key field", `{"private_key":"short"}`, "short"},
	} {
		t.Run(test.name, func(t *testing.T) {
			redacted, spans, err := RedactText(test.input)
			if err != nil {
				t.Fatalf("RedactText() error = %v", err)
			}
			if strings.Contains(redacted, test.secret) {
				t.Fatalf("secret %q survived redaction: %s", test.secret, redacted)
			}
			if len(spans) == 0 {
				t.Fatalf("redaction reported no spans for %q", test.input)
			}
			if !strings.Contains(redacted, "[REDACTED]") {
				t.Fatalf("redacted text lost its placeholder: %s", redacted)
			}
			if err := ValidateJSON([]byte(test.input)); err == nil {
				t.Fatalf("opaque value passed validation: %q", test.input)
			}
			if err := Validate(map[string]string{"excerpt": redacted}); err != nil {
				t.Fatalf("redacted value failed the public-data gate: %v", err)
			}
		})
	}
}

func TestBareSensitiveKeysPreserveOrdinaryProse(t *testing.T) {
	for _, input := range []string{
		"QA test session: verify consultation review readback; no model submission.",
		"the token: rotate later",
		"email me when ready",
		"session: review_later",
		`{"session":"review_later"}`,
		`{"secret":"keep"}`,
	} {
		t.Run(input, func(t *testing.T) {
			redacted, spans, err := RedactText(input)
			if err != nil {
				t.Fatalf("RedactText() error = %v", err)
			}
			if redacted != input || len(spans) != 0 {
				t.Fatalf("prose changed: got %q with spans %#v", redacted, spans)
			}
			if err := Validate(map[string]string{"excerpt": input}); err != nil {
				t.Fatalf("ordinary prose failed validation: %v", err)
			}
			if strings.HasPrefix(input, "{") {
				if err := ValidateJSON([]byte(input)); err != nil {
					t.Fatalf("ordinary JSON prose failed validation: %v", err)
				}
			}
		})
	}
}
