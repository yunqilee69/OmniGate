package store

import (
	"strings"
	"testing"
)

func TestParseHeaderProfiles(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []HeaderProfile
		wantErr string
	}{
		{"empty string", "", nil, ""},
		{"whitespace only", "   \n ", nil, ""},
		{"empty array", `[]`, []HeaderProfile{}, ""},
		{"single group",
			`[{"name":"cli","headers":{"User-Agent":"cli/1","X-App":"web"}}]`,
			[]HeaderProfile{{Name: "cli", Headers: map[string]string{"User-Agent": "cli/1", "X-App": "web"}}},
			""},
		{"invalid json", `{bad`, nil, "JSON 非法"},
		{"json not array", `"x"`, nil, "JSON 非法"},
		{"empty name", `[{"headers":{"A":"b"}}]`, nil, "组名为空"},
		{"blank name", `[{"name":"  ","headers":{}}]`, nil, "组名为空"},
		{"duplicate name", `[{"name":"a","headers":{}},{"name":"a","headers":{}}]`, nil, "组名重复"},
		{"empty header key", `[{"name":"a","headers":{"":"v"}}]`, nil, "空 header key"},
		{"blank header key", `[{"name":"a","headers":{"  ":"v"}}]`, nil, "空 header key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseHeaderProfiles(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseHeaderProfiles(%q) error = %v, want contains %q", tt.raw, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseHeaderProfiles(%q) unexpected error: %v", tt.raw, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d groups, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i].Name != tt.want[i].Name {
					t.Errorf("group[%d].name = %q, want %q", i, got[i].Name, tt.want[i].Name)
				}
				for k, v := range tt.want[i].Headers {
					if got[i].Headers[k] != v {
						t.Errorf("group[%d].headers[%q] = %q, want %q", i, k, got[i].Headers[k], v)
					}
				}
			}
		})
	}
}

func TestReservedHeaderKeysCoverAuthAndTransport(t *testing.T) {
	for _, k := range []string{"authorization", "x-api-key", "host", "content-length", "content-type", "accept-encoding"} {
		if !ReservedHeaderKeys[k] {
			t.Errorf("ReservedHeaderKeys missing %q", k)
		}
	}
}
