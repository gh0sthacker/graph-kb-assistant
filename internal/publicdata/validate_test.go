package publicdata

import "testing"

func TestValidateJSONRejectsPrivateAndSecretLikeGraphData(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"private request field", `{"raw_request":"GET /private HTTP/1.1"}`},
		{"secret assignment", `{"description":"password=secret-value"}`},
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

func TestValidateAllowsRedactedValuesAndShapeOnlyUrls(t *testing.T) {
	input := []byte(`{"token":"[REDACTED]","url":"https://example.test/path?token=%5BREDACTED%5D","fields":[{"name":"id","value":""}]}`)
	if err := ValidateJSON(input); err != nil {
		t.Fatalf("valid redacted metadata was rejected: %v", err)
	}
}
