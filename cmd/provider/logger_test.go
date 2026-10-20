package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJSONFormatWithDebugKeepsDebugLinesAsJSON(t *testing.T) {
	var buf bytes.Buffer
	newZapLogger(true, "json", &buf).V(1).Info("observed", "owner_org_id", "org-a")

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("debug line is not JSON: %q", buf.String())
	}
	if line["owner_org_id"] != "org-a" {
		t.Fatalf("owner_org_id missing from %v", line)
	}
}

func TestJSONFormatWithoutDebugDropsDebugLines(t *testing.T) {
	var buf bytes.Buffer
	newZapLogger(false, "json", &buf).V(1).Info("observed")

	if buf.Len() != 0 {
		t.Fatalf("debug line emitted without --debug: %q", buf.String())
	}
}

func TestConsoleFormatIsNotJSON(t *testing.T) {
	var buf bytes.Buffer
	newZapLogger(true, "console", &buf).Info("observed")

	if strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Fatalf("console format produced JSON: %q", buf.String())
	}
}

// Other controllers writing to the same log index emit ts as an RFC3339
// string; a numeric ts would clash with that field's mapping.
func TestJSONTimestampIsAnRFC3339String(t *testing.T) {
	var buf bytes.Buffer
	newZapLogger(false, "json", &buf).Info("observed")

	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("line is not JSON: %q", buf.String())
	}
	ts, ok := line["ts"].(string)
	if !ok {
		t.Fatalf("ts = %#v, want an RFC3339 string", line["ts"])
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Fatalf("ts %q is not RFC3339: %v", ts, err)
	}
}
