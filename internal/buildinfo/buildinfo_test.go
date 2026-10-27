package buildinfo

import (
	"regexp"
	"strings"
	"testing"
)

func TestStringIncludesShortRevisionWhenAvailable(t *testing.T) {
	value := String()
	if !strings.HasPrefix(value, version) {
		t.Fatalf("String() = %q, want prefix %q", value, version)
	}
	info := Current()
	if info.Revision == "" {
		if value != version {
			t.Fatalf("String() without VCS metadata = %q, want %q", value, version)
		}
		return
	}
	if !regexp.MustCompile(`^0\.1\.0-go-dev\+[a-f0-9]{12}(\.dirty)?$`).MatchString(value) {
		t.Fatalf("String() = %q, want commit-qualified version", value)
	}
}
