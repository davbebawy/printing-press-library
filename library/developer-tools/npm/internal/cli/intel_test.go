package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageSummaryCommand(t *testing.T) {
	withMockNPM(t, func() {
		var out bytes.Buffer
		root := RootCmd()
		root.SetOut(&out)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"package", "left-pad", "--json", "--no-cache"})
		if err := root.Execute(); err != nil {
			t.Fatalf("execute package: %v", err)
		}

		var got packageSummary
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out.String())
		}
		if got.Name != "left-pad" || got.LatestVersion != "1.3.0" {
			t.Fatalf("unexpected package summary: %+v", got)
		}
		if got.LastMonthDownloads != 123456 {
			t.Fatalf("downloads: want 123456, got %d", got.LastMonthDownloads)
		}
		if got.DependencyCount != 2 || got.MaintainerCount != 1 {
			t.Fatalf("unexpected counts: %+v", got)
		}
	})
}

func TestCompareCommandRanksPackagesByDownloads(t *testing.T) {
	withMockNPM(t, func() {
		var out bytes.Buffer
		root := RootCmd()
		root.SetOut(&out)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"compare", "left-pad", "tiny-lib", "--json", "--no-cache"})
		if err := root.Execute(); err != nil {
			t.Fatalf("execute compare: %v", err)
		}

		var got []packageSummary
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out.String())
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 packages, got %d", len(got))
		}
		if got[0].Name != "left-pad" || got[1].Name != "tiny-lib" {
			t.Fatalf("expected download-ranked packages, got %+v", got)
		}
	})
}

func TestRiskCommandFlagsStaleAndMissingLicense(t *testing.T) {
	withMockNPM(t, func() {
		var out bytes.Buffer
		root := RootCmd()
		root.SetOut(&out)
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"risk", "tiny-lib", "--json", "--no-cache"})
		if err := root.Execute(); err != nil {
			t.Fatalf("execute risk: %v", err)
		}

		var got packageRisk
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("decode output: %v\n%s", err, out.String())
		}
		if got.Name != "tiny-lib" || got.Score < 40 {
			t.Fatalf("unexpected risk score: %+v", got)
		}
		joined := strings.Join(got.Signals, " ")
		if !strings.Contains(joined, "missing license") || !strings.Contains(joined, "stale release") {
			t.Fatalf("expected stale and missing-license signals, got %+v", got.Signals)
		}
	})
}

func withMockNPM(t *testing.T, run func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/left-pad":
			_, _ = w.Write([]byte(`{
				"name":"left-pad",
				"description":"String left pad",
				"dist-tags":{"latest":"1.3.0"},
				"license":"WTFPL",
				"keywords":["string","pad"],
				"maintainers":[{"name":"alice"}],
				"versions":{"1.3.0":{"dependencies":{"a":"1.0.0","b":"1.0.0"}}},
				"time":{"1.3.0":"2026-05-01T00:00:00.000Z"}
			}`))
		case "/tiny-lib":
			_, _ = w.Write([]byte(`{
				"name":"tiny-lib",
				"description":"Tiny library",
				"dist-tags":{"latest":"0.1.0"},
				"maintainers":[],
				"versions":{"0.1.0":{"dependencies":{}}},
				"time":{"0.1.0":"2021-01-01T00:00:00.000Z"}
			}`))
		case "/downloads/point/last-month/left-pad":
			_, _ = w.Write([]byte(`{"downloads":123456,"package":"left-pad"}`))
		case "/downloads/point/last-month/tiny-lib":
			_, _ = w.Write([]byte(`{"downloads":17,"package":"tiny-lib"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("NPM_BASE_URL", server.URL)
	t.Setenv("NPM_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("HOME", t.TempDir())
	oldArgs := os.Args
	os.Args = []string{"npm-pp-cli"}
	defer func() { os.Args = oldArgs }()

	run()
}
