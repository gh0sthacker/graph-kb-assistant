package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeReconFixture(t *testing.T, dir string, n, m, shells int, wrapped bool) {
	t.Helper()
	routes := make([]map[string]any, n)
	for i := range routes {
		hash := fmt.Sprintf("body-%d", i)
		if i < shells {
			hash = "shell"
		}
		routes[i] = map[string]any{"url": fmt.Sprintf("/route/%d", i), "body_sha256": hash, "content_type": "text/html; charset=utf-8"}
	}
	points := make([]map[string]any, m)
	for i := range points {
		points[i] = map[string]any{"name": fmt.Sprintf("param-%d", i)}
	}
	var r, p any = routes, points
	if wrapped {
		r = map[string]any{"routes": routes}
		p = map[string]any{"input_points": points}
	}
	for file, value := range map[string]any{"routes.json": r, "input-points.json": p} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReconProbe(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		n, m, shells, minN, minM int
		wrapped                  bool
		want                     Verdict
	}{
		{"all OK", 20, 5, 0, 0, 0, false, Valid},
		{"only 4 routes", 4, 5, 0, 0, 0, false, Invalid},
		{"missing inputs", 20, 4, 0, 0, 0, false, Invalid},
		{"SPA majority", 20, 5, 11, 0, 0, false, Invalid},
		{"SPA exactly half", 20, 5, 10, 0, 0, false, Invalid},
		{"SPA minority", 20, 5, 9, 0, 0, false, Valid},
		{"custom thresholds wrapped", 4, 2, 0, 4, 2, true, Valid},
		{"negative thresholds", 20, 5, 0, -1, 5, false, Invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeReconFixture(t, dir, tc.n, tc.m, tc.shells, tc.wrapped)
			got := New(ReconProbe(ReconConfig{Dir: dir, MinRoutes: tc.minN, MinInputPoints: tc.minM})).Check(context.Background())
			if got.Verdict != tc.want {
				t.Fatalf("result = %+v", got)
			}
		})
	}
}

func TestReconRejectsMalformedOrMissingData(t *testing.T) {
	for _, body := range []string{"", `null`, `{}`, `{"count":95}`, `[null]`, `[{}]`, `[1]`, `[""]`, `[] trailing`, `{"routes":null}`} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			writeReconFixture(t, dir, 20, 5, 0, false)
			if err := os.WriteFile(filepath.Join(dir, "routes.json"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			got := New(ReconProbe(ReconConfig{Dir: dir, MinRoutes: 1})).Check(context.Background())
			if got.Verdict != Invalid {
				t.Fatalf("result = %+v", got)
			}
		})
	}
	for _, dir := range []string{"", t.TempDir()} {
		got := New(ReconProbe(ReconConfig{Dir: dir})).Check(context.Background())
		if got.Verdict != Invalid {
			t.Fatalf("result = %+v", got)
		}
	}
}

func TestReconOptionalFingerprints(t *testing.T) {
	for _, tc := range []struct {
		name, ctype string
		want        Verdict
	}{
		{"JSON responses not SPA", "application/json", Valid},
		{"hash without type conservatively checked", "", Invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeReconFixture(t, dir, 20, 5, 20, false)
			data, err := os.ReadFile(filepath.Join(dir, "routes.json"))
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.ReplaceAll(string(data), "text/html; charset=utf-8", tc.ctype))
			if err := os.WriteFile(filepath.Join(dir, "routes.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			got := New(ReconProbe(ReconConfig{Dir: dir})).Check(context.Background())
			if got.Verdict != tc.want {
				t.Fatalf("result = %+v", got)
			}
		})
	}
	dir := t.TempDir()
	writeReconFixture(t, dir, 20, 5, 0, false)
	routes := make([]string, 20)
	for i := range routes {
		routes[i] = fmt.Sprintf("/route/%d", i)
	}
	data, _ := json.Marshal(routes)
	if err := os.WriteFile(filepath.Join(dir, "routes.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if got := New(ReconProbe(ReconConfig{Dir: dir})).Check(context.Background()); got.Verdict != Valid {
		t.Fatalf("no fingerprints = %+v", got)
	}
}
