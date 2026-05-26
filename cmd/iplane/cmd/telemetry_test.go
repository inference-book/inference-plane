package cmd

import (
	"strings"
	"testing"
)

// discoverCloudflaredFromLogs is the pure-buffer core of the docker-
// logs scraper, so we can test the regex without spawning docker.
func TestDiscoverCloudflaredFromLogs_HappyPath(t *testing.T) {
	logs := []byte(`
2026-05-25T18:00:01Z INF Starting tunnel
2026-05-25T18:00:02Z INF Version 2025.5.0
2026-05-25T18:00:03Z INF +-------------------------------------+
2026-05-25T18:00:03Z INF |  Your quick Tunnel has been created |
2026-05-25T18:00:03Z INF |  https://random-words-1234.trycloudflare.com  |
2026-05-25T18:00:03Z INF +-------------------------------------+
`)
	url, err := discoverCloudflaredFromLogs(logs)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if url != "https://random-words-1234.trycloudflare.com" {
		t.Errorf("url = %q, want the trycloudflare URL", url)
	}
}

func TestDiscoverCloudflaredFromLogs_NoBanner(t *testing.T) {
	logs := []byte("2026-05-25T18:00:00Z INF Starting tunnel\n")
	_, err := discoverCloudflaredFromLogs(logs)
	if err == nil {
		t.Fatal("expected error when banner is missing")
	}
	if !strings.Contains(err.Error(), "still be starting") {
		t.Errorf("error should hint that cloudflared is still starting; got: %v", err)
	}
}

func TestDiscoverCloudflaredFromLogs_BannerButNoURL(t *testing.T) {
	// Format drift: banner present, URL not. Surface verbatim so the
	// operator can read the actual log line and we can update the regex.
	logs := []byte(`
INF Your quick Tunnel has been created
INF Visit https://example.com for help
`)
	_, err := discoverCloudflaredFromLogs(logs)
	if err == nil {
		t.Fatal("expected error when URL is missing despite banner")
	}
	if !strings.Contains(err.Error(), "format may have changed") {
		t.Errorf("error should mention format drift; got: %v", err)
	}
}

func TestMergeOtelEnv_FlagsOverrideEmptyBase(t *testing.T) {
	got := mergeOtelEnv(nil, "https://otlp.example.com",
		map[string]string{"Authorization": "Basic xyz"})
	if got["OTEL_EXPORTER_OTLP_ENDPOINT"] != "https://otlp.example.com" {
		t.Errorf("endpoint not set: %+v", got)
	}
	if got["OTEL_EXPORTER_OTLP_HEADERS"] != "Authorization=Basic xyz" {
		t.Errorf("headers not set: %+v", got)
	}
}

func TestMergeOtelEnv_BaseEnvWins(t *testing.T) {
	// Operator's explicit --env overrides what --otel-endpoint would
	// set. Power-user escape hatch.
	got := mergeOtelEnv(
		map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://override.example.com"},
		"https://flag.example.com",
		nil,
	)
	if got["OTEL_EXPORTER_OTLP_ENDPOINT"] != "https://override.example.com" {
		t.Errorf("explicit --env should override --otel-endpoint; got: %+v", got)
	}
}

func TestMergeOtelEnv_NilWhenAllEmpty(t *testing.T) {
	got := mergeOtelEnv(nil, "", nil)
	if got != nil {
		t.Errorf("want nil when no env / flags / headers; got %+v", got)
	}
}

func TestMergeOtelEnv_HeadersJoinedWithCommas(t *testing.T) {
	got := mergeOtelEnv(nil, "https://x", map[string]string{
		"Authorization": "Basic abc",
		"x-tenant":      "42",
	})
	hdr := got["OTEL_EXPORTER_OTLP_HEADERS"]
	// Order is non-deterministic (map iteration); both pairs must
	// appear and be comma-separated.
	if !strings.Contains(hdr, "Authorization=Basic abc") || !strings.Contains(hdr, "x-tenant=42") {
		t.Errorf("missing pair in header value: %q", hdr)
	}
	if !strings.Contains(hdr, ",") {
		t.Errorf("multi-header value should be comma-separated: %q", hdr)
	}
}

func TestParseOtelHeadersEnv(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
	}{
		{"", nil},
		{"Authorization=Basic xyz", map[string]string{"Authorization": "Basic xyz"}},
		{" Authorization=Basic xyz ,  x-tenant=42 ", map[string]string{"Authorization": "Basic xyz", "x-tenant": "42"}},
		{"malformed-no-equals", nil},
		{"=no-key", nil},
	}
	for _, c := range cases {
		got := parseOtelHeadersEnv(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseOtelHeadersEnv(%q) len = %d, want %d (got %+v)", c.in, len(got), len(c.want), got)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("parseOtelHeadersEnv(%q)[%q] = %q, want %q", c.in, k, got[k], v)
			}
		}
	}
}
