package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
	"wiki-go/internal/config"
)

func bearerRequest(token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func installTokenConfig(t *testing.T, users []config.User) {
	t.Helper()
	previous := tokenConfig.Load()
	t.Cleanup(func() { tokenConfig.Store(previous) })
	cfg := &config.Config{}
	cfg.Users = users
	SetConfig(cfg)
}

func TestGetSessionAcceptsSessionTokenAsBearer(t *testing.T) {
	if err := InitSessionStore(filepath.Join(t.TempDir(), "sessions.json")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Server.AllowInsecureCookies = true
	cookie := createTestSession(t, cfg, "alice")

	session := GetSession(bearerRequest(cookie.Value))
	if session == nil || session.Username != "alice" {
		t.Fatalf("bearer session = %+v, want alice", session)
	}
	if session.IsPersonalToken() {
		t.Fatal("login session reported as a personal access token")
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Basic "+cookie.Value)
	if GetSession(request) != nil {
		t.Fatal("non-bearer authorization scheme was accepted")
	}
}

func TestGetSessionResolvesPersonalTokens(t *testing.T) {
	adminToken, adminHash, err := GeneratePersonalToken()
	if err != nil {
		t.Fatal(err)
	}
	cappedToken, cappedHash, _ := GeneratePersonalToken()
	expiredToken, expiredHash, _ := GeneratePersonalToken()
	unknownToken, _, _ := GeneratePersonalToken()

	installTokenConfig(t, []config.User{{
		Username: "alice",
		Role:     config.RoleAdmin,
		Groups:   []string{"finance"},
		Tokens: []config.APIToken{
			{Name: "full", Hash: adminHash},
			{Name: "reader", Hash: cappedHash, Role: config.RoleViewer},
			{Name: "old", Hash: expiredHash, ExpiresAt: time.Now().Add(-time.Hour)},
		},
	}})

	session := GetSession(bearerRequest(adminToken))
	if session == nil || session.Username != "alice" || session.Role != config.RoleAdmin {
		t.Fatalf("full token session = %+v, want alice as admin", session)
	}
	if session.TokenName != "full" || len(session.Groups) != 1 || session.Groups[0] != "finance" {
		t.Fatalf("full token session = %+v, want token name and owner groups", session)
	}
	if !RequireRole(bearerRequest(adminToken), config.RoleAdmin) {
		t.Fatal("RequireRole rejected an admin personal token")
	}

	if session := GetSession(bearerRequest(cappedToken)); session == nil || session.Role != config.RoleViewer {
		t.Fatalf("capped token session = %+v, want viewer", session)
	}
	if RequireRole(bearerRequest(cappedToken), config.RoleEditor) {
		t.Fatal("viewer-capped token passed an editor check")
	}

	if GetSession(bearerRequest(expiredToken)) != nil {
		t.Fatal("expired token was accepted")
	}
	if GetSession(bearerRequest(unknownToken)) != nil {
		t.Fatal("unknown token was accepted")
	}
}

func TestPersonalTokenFollowsOwnerChanges(t *testing.T) {
	token, hash, err := GeneratePersonalToken()
	if err != nil {
		t.Fatal(err)
	}
	installTokenConfig(t, []config.User{{
		Username: "alice",
		Role:     config.RoleEditor,
		Tokens:   []config.APIToken{{Name: "agent", Hash: hash, Role: config.RoleAdmin}},
	}})

	if session := GetSession(bearerRequest(token)); session == nil || session.Role != config.RoleEditor {
		t.Fatalf("session = %+v, want the token capped to the owner's editor role", session)
	}

	cfg := tokenConfig.Load()
	cfg.Users[0].Role = config.RoleViewer
	SetConfig(cfg)
	if session := GetSession(bearerRequest(token)); session == nil || session.Role != config.RoleViewer {
		t.Fatalf("session = %+v, want the owner's new viewer role", session)
	}

	cfg = tokenConfig.Load()
	cfg.Users[0].Tokens = nil
	SetConfig(cfg)
	if GetSession(bearerRequest(token)) != nil {
		t.Fatal("removed token was still accepted")
	}
}
