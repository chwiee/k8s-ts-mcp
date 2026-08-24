package redact

import (
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		wantContains string
		wantGone     string
	}{
		{"aws access key", "export AWS_ACCESS_KEY_ID=AKIAABCDEFGHIJKLMNOP", Placeholder, "AKIAABCDEFGHIJKLMNOP"},
		{"bearer token", "Authorization: Bearer abc123.def456-ghi789", Placeholder, "abc123.def456-ghi789"},
		{"jwt", "token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", Placeholder, "eyJzdWIiOiIxIn0"},
		{"plain text untouched", "pod nginx-abc123 CrashLoopBackOff", "CrashLoopBackOff", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Text(tc.in)
			if tc.wantGone != "" && strings.Contains(got, tc.wantGone) {
				t.Errorf("Text(%q) = %q, still contains secret %q", tc.in, got, tc.wantGone)
			}
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("Text(%q) = %q, want it to contain %q", tc.in, got, tc.wantContains)
			}
		})
	}
}

func TestObjectStripsSecretData(t *testing.T) {
	in := map[string]any{
		"kind": "Secret",
		"metadata": map[string]any{
			"name": "db-credentials",
		},
		"data": map[string]any{
			"password": "cGxhaW50ZXh0LXBhc3N3b3Jk",
		},
	}
	out := Object(in).(map[string]any)
	if _, ok := out["data"]; ok {
		t.Errorf("Object() kept %q key, want it stripped", "data")
	}
	meta := out["metadata"].(map[string]any)
	if meta["name"] != "db-credentials" {
		t.Errorf("Object() dropped non-sensitive field, got metadata=%v", meta)
	}
}

func TestObjectScrubsNestedStrings(t *testing.T) {
	in := map[string]any{
		"log": "auth failed for Bearer abc123.def456-ghi789",
	}
	out := Object(in).(map[string]any)
	if strings.Contains(out["log"].(string), "abc123.def456-ghi789") {
		t.Errorf("Object() left a token inside a nested string: %v", out)
	}
}
