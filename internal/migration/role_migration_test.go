package migration

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"wiki-go/internal/config"
	"wiki-go/internal/roles"

	"gopkg.in/yaml.v3"
)

const legacyRoleConfig = `server:
    host: "127.0.0.1"
    port: 9090
    trusted_proxies: ["10.0.0.1"]
wiki:
    root_dir: "data"
    title: "Team Wiki"
    private: false
    enable_link_embedding: true
security:
    # cost factor for bcrypt password hashing
    passwordstrength: 12
    login_ban:
        enabled: true
        max_failures: 3
users:
    - username: admin
      password: $2a$10$adminhash
      is_admin: true
    - username: bob
      password: $2a$10$bobhash
      groups:
        - finance-team
    - username: carol
      password: $2a$10$carolhash
      role: editor
access_rules:
    - pattern: "/finance/**"
      access: restricted
      groups: [finance-team]
      description: "Finance only"
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigrateUserRolesPreservesConfig(t *testing.T) {
	path := writeConfig(t, legacyRoleConfig)

	if err := MigrateUserRoles(path); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("migrated config is invalid YAML: %v\n%s", err, data)
	}

	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 9090 {
		t.Fatalf("server = %s:%d, want 127.0.0.1:9090", cfg.Server.Host, cfg.Server.Port)
	}
	if !slices.Equal(cfg.Server.TrustedProxies, []string{"10.0.0.1"}) {
		t.Fatalf("trusted proxies = %#v", cfg.Server.TrustedProxies)
	}
	if cfg.Wiki.Title != "Team Wiki" || !cfg.Wiki.EnableLinkEmbedding {
		t.Fatalf("wiki settings lost: title=%q enable_link_embedding=%t", cfg.Wiki.Title, cfg.Wiki.EnableLinkEmbedding)
	}
	if cfg.Security.PasswordStrength != 12 || cfg.Security.LoginBan.MaxFailures != 3 {
		t.Fatalf("security settings lost: %+v", cfg.Security)
	}

	if len(cfg.AccessRules) != 1 {
		t.Fatalf("access rules = %#v, want 1 rule", cfg.AccessRules)
	}
	rule := cfg.AccessRules[0]
	if rule.Pattern != "/finance/**" || rule.Access != "restricted" || !slices.Equal(rule.Groups, []string{"finance-team"}) {
		t.Fatalf("access rule = %#v", rule)
	}

	want := []config.User{
		{Username: "admin", Password: "$2a$10$adminhash", Role: roles.RoleAdmin},
		{Username: "bob", Password: "$2a$10$bobhash", Role: roles.RoleViewer, Groups: []string{"finance-team"}},
		{Username: "carol", Password: "$2a$10$carolhash", Role: roles.RoleEditor},
	}
	if len(cfg.Users) != len(want) {
		t.Fatalf("users = %#v, want %#v", cfg.Users, want)
	}
	for i, user := range cfg.Users {
		if user.Username != want[i].Username || user.Password != want[i].Password ||
			user.Role != want[i].Role || !slices.Equal(user.Groups, want[i].Groups) {
			t.Fatalf("user %d = %#v, want %#v", i, user, want[i])
		}
	}

	if bytes.Contains(data, []byte("is_admin")) {
		t.Fatalf("is_admin was not removed:\n%s", data)
	}
	if !bytes.Contains(data, []byte("# cost factor for bcrypt password hashing")) {
		t.Fatalf("comments were not preserved:\n%s", data)
	}

	backups, err := filepath.Glob(path + ".*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != legacyRoleConfig {
		t.Fatalf("backup does not match the original config:\n%s", backup)
	}
}

func TestMigrateUserRolesLeavesMigratedConfigUntouched(t *testing.T) {
	original := strings.Replace(legacyRoleConfig, "      is_admin: true\n", "      role: admin\n", 1)
	original = strings.Replace(original, "        - finance-team\n    - username: carol",
		"        - finance-team\n      role: viewer\n    - username: carol", 1)
	path := writeConfig(t, original)

	if err := MigrateUserRoles(path); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("config changed although no migration was needed:\n%s", data)
	}
	backups, err := filepath.Glob(path + ".*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 0 {
		t.Fatalf("unexpected backups: %v", backups)
	}
}
