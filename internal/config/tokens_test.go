package config

import (
	"bytes"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestSaveConfigPreservesUserTokens(t *testing.T) {
	created := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cfg := &Config{}
	cfg.Users = []User{{
		Username: "alice",
		Password: "hash",
		Role:     RoleEditor,
		Tokens: []APIToken{
			{Name: "mcp-agent", Hash: "abc123", Role: RoleViewer, CreatedAt: created, ExpiresAt: created.AddDate(0, 0, 30)},
			{Name: "ci", Hash: "def456", CreatedAt: created},
		},
	}}

	var output bytes.Buffer
	if err := SaveConfig(cfg, &output); err != nil {
		t.Fatal(err)
	}

	var decoded Config
	if err := yaml.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("generated config is invalid YAML: %v\n%s", err, output.String())
	}
	if len(decoded.Users) != 1 || len(decoded.Users[0].Tokens) != 2 {
		t.Fatalf("decoded users = %+v, want one user with two tokens\n%s", decoded.Users, output.String())
	}
	for i, want := range cfg.Users[0].Tokens {
		got := decoded.Users[0].Tokens[i]
		if got.Name != want.Name || got.Hash != want.Hash || got.Role != want.Role ||
			!got.CreatedAt.Equal(want.CreatedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
			t.Fatalf("token %d = %+v, want %+v", i, got, want)
		}
	}
}
