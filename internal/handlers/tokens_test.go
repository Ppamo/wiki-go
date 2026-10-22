package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"wiki-go/internal/auth"
	"wiki-go/internal/config"
)

func installTokenTestConfig(t *testing.T, users []config.User) *config.Config {
	t.Helper()
	testConfig := installUserSessionTestConfig(t, users)
	auth.SetConfig(testConfig)
	t.Cleanup(func() { auth.SetConfig(nil) })
	return testConfig
}

func tokensRequest(method, target, body string, credential func(*http.Request)) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	credential(request)
	response := httptest.NewRecorder()
	TokensHandler(response, request)
	return response
}

func withCookie(cookie *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(cookie) }
}

func withBearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func createTestToken(t *testing.T, cookie *http.Cookie, body string) string {
	t.Helper()
	response := tokensRequest(http.MethodPost, "/api/tokens", body, withCookie(cookie))
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body: %s", response.Code, response.Body.String())
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Token, auth.PersonalTokenPrefix) {
		t.Fatalf("token = %q, want prefix %q", result.Token, auth.PersonalTokenPrefix)
	}
	return result.Token
}

func checkAuthWithBearer(token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/api/check-auth", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	CheckAuthHandler(response, request)
	return response
}

func TestTokensHandlerCreatesUsableTokenAndStoresOnlyItsHash(t *testing.T) {
	testConfig := installTokenTestConfig(t, []config.User{
		userWithPassword(t, "alice", "password", config.RoleEditor),
	})
	cookie := sessionCookieForUser(t, testConfig, "alice", config.RoleEditor)

	token := createTestToken(t, cookie, `{"name":"mcp-agent","role":"viewer","expires_in_days":30}`)

	response := checkAuthWithBearer(token)
	if response.Code != http.StatusOK {
		t.Fatalf("check-auth status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	var result struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Username != "alice" || result.Role != config.RoleViewer {
		t.Fatalf("check-auth = %+v, want alice as viewer", result)
	}

	saved, err := os.ReadFile(config.ConfigFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), token) {
		t.Fatal("plain token was written to the config file")
	}
	loaded, err := config.LoadConfig(config.ConfigFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Users) != 1 || len(loaded.Users[0].Tokens) != 1 || loaded.Users[0].Tokens[0].Name != "mcp-agent" {
		t.Fatalf("reloaded users = %+v, want alice with the mcp-agent token", loaded.Users)
	}

	list := tokensRequest(http.MethodGet, "/api/tokens", "", withCookie(cookie))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"name":"mcp-agent"`) {
		t.Fatalf("list status = %d; body: %s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), testConfig.Users[0].Tokens[0].Hash) {
		t.Fatal("token list exposed the stored hash")
	}
}

func TestTokensHandlerRejectsInvalidRequests(t *testing.T) {
	testConfig := installTokenTestConfig(t, []config.User{
		userWithPassword(t, "alice", "password", config.RoleEditor),
	})
	cookie := sessionCookieForUser(t, testConfig, "alice", config.RoleEditor)
	token := createTestToken(t, cookie, `{"name":"agent"}`)

	tests := []struct {
		name       string
		method     string
		target     string
		body       string
		credential func(*http.Request)
		want       int
	}{
		{"anonymous", http.MethodGet, "/api/tokens", "", func(*http.Request) {}, http.StatusUnauthorized},
		{"token lists tokens", http.MethodGet, "/api/tokens", "", withBearer(token), http.StatusForbidden},
		{"token creates token", http.MethodPost, "/api/tokens", `{"name":"other"}`, withBearer(token), http.StatusForbidden},
		{"token deletes itself", http.MethodDelete, "/api/tokens?name=agent", "", withBearer(token), http.StatusForbidden},
		{"role above owner", http.MethodPost, "/api/tokens", `{"name":"root","role":"admin"}`, withCookie(cookie), http.StatusForbidden},
		{"unknown role", http.MethodPost, "/api/tokens", `{"name":"x","role":"owner"}`, withCookie(cookie), http.StatusBadRequest},
		{"bad name", http.MethodPost, "/api/tokens", `{"name":"has space"}`, withCookie(cookie), http.StatusBadRequest},
		{"negative expiry", http.MethodPost, "/api/tokens", `{"name":"x","expires_in_days":-1}`, withCookie(cookie), http.StatusBadRequest},
		{"duplicate name", http.MethodPost, "/api/tokens", `{"name":"agent"}`, withCookie(cookie), http.StatusConflict},
		{"delete unknown", http.MethodDelete, "/api/tokens?name=missing", "", withCookie(cookie), http.StatusNotFound},
		{"bad method", http.MethodPut, "/api/tokens", "", withCookie(cookie), http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := tokensRequest(test.method, test.target, test.body, test.credential)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body: %s", response.Code, test.want, response.Body.String())
			}
		})
	}
	if len(testConfig.Users[0].Tokens) != 1 {
		t.Fatalf("tokens = %+v, want only the original token", testConfig.Users[0].Tokens)
	}
}

func TestDeletedTokenStopsWorking(t *testing.T) {
	testConfig := installTokenTestConfig(t, []config.User{
		userWithPassword(t, "alice", "password", config.RoleEditor),
	})
	cookie := sessionCookieForUser(t, testConfig, "alice", config.RoleEditor)
	token := createTestToken(t, cookie, `{"name":"agent"}`)

	response := tokensRequest(http.MethodDelete, "/api/tokens?name=agent", "", withCookie(cookie))
	if response.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	if checkAuthWithBearer(token).Code != http.StatusUnauthorized {
		t.Fatal("deleted token was still accepted")
	}
}

func TestConcurrentTokenCreationPreservesEveryToken(t *testing.T) {
	testConfig := installTokenTestConfig(t, []config.User{
		userWithPassword(t, "alice", "password", config.RoleEditor),
	})
	cookie := sessionCookieForUser(t, testConfig, "alice", config.RoleEditor)

	const tokenCount = 10
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, tokenCount)
	var wg sync.WaitGroup
	for i := 0; i < tokenCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			body := fmt.Sprintf(`{"name":"agent-%d"}`, index)
			responses <- tokensRequest(http.MethodPost, "/api/tokens", body, withCookie(cookie))
		}(i)
	}
	close(start)
	wg.Wait()
	close(responses)

	for response := range responses {
		if response.Code != http.StatusCreated {
			t.Fatalf("create status = %d, want 201; body: %s", response.Code, response.Body.String())
		}
	}
	if len(testConfig.Users[0].Tokens) != tokenCount {
		t.Fatalf("stored tokens = %d, want %d", len(testConfig.Users[0].Tokens), tokenCount)
	}

	loaded, err := config.LoadConfig(config.ConfigFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Users) != 1 || len(loaded.Users[0].Tokens) != tokenCount {
		t.Fatalf("persisted tokens = %+v, want %d tokens", loaded.Users, tokenCount)
	}
}

func TestPasswordChangeRevokesPersonalTokens(t *testing.T) {
	testConfig := installTokenTestConfig(t, []config.User{
		userWithPassword(t, "alice", "old-password", config.RoleEditor),
	})
	cookie := sessionCookieForUser(t, testConfig, "alice", config.RoleEditor)
	token := createTestToken(t, cookie, `{"name":"agent"}`)

	request := httptest.NewRequest(http.MethodPost, "/api/password", strings.NewReader(`{
		"current_password":"old-password",
		"new_password":"new-password"
	}`))
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	PasswordHandler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("password status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	if checkAuthWithBearer(token).Code != http.StatusUnauthorized {
		t.Fatal("token survived a password change")
	}
}
