package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"wiki-go/internal/auth"
	"wiki-go/internal/config"
)

type testWiki struct {
	cfg          *config.Config
	server       http.Handler
	viewerToken  string
	financeToken string
}

func writeDocument(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func newTestWiki(t *testing.T) *testWiki {
	t.Helper()
	root := t.TempDir()
	writeDocument(t, filepath.Join(root, "pages", "home", "document.md"), "# Home")
	writeDocument(t, filepath.Join(root, "documents", "guides", "intro", "document.md"), "# Intro")
	writeDocument(t, filepath.Join(root, "documents", "finance", "report", "document.md"), "# Report")
	writeDocument(t, filepath.Join(root, "config.yaml"), "secret: true")

	viewerToken, viewerHash, err := auth.GeneratePersonalToken()
	if err != nil {
		t.Fatal(err)
	}
	financeToken, financeHash, _ := auth.GeneratePersonalToken()

	cfg := &config.Config{}
	cfg.Server.AllowInsecureCookies = true
	cfg.Wiki.RootDir = root
	cfg.Wiki.DocumentsDir = "documents"
	cfg.AccessRules = []config.AccessRule{{
		Pattern: "/finance/**",
		Access:  "restricted",
		Groups:  []string{"finance"},
	}}
	cfg.Users = []config.User{
		{Username: "alice", Role: config.RoleViewer, Tokens: []config.APIToken{{Name: "agent", Hash: viewerHash}}},
		{Username: "bob", Role: config.RoleEditor, Groups: []string{"finance"}, Tokens: []config.APIToken{{Name: "agent", Hash: financeHash}}},
	}

	if err := auth.InitSessionStore(filepath.Join(root, "sessions.json")); err != nil {
		t.Fatal(err)
	}
	auth.SetConfig(cfg)
	t.Cleanup(func() { auth.SetConfig(nil) })

	return &testWiki{cfg: cfg, server: NewServer(cfg), viewerToken: viewerToken, financeToken: financeToken}
}

func (w *testWiki) post(t *testing.T, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	w.server.ServeHTTP(response, request)
	return response
}

type rpcResult struct {
	Result struct {
		ProtocolVersion string                 `json:"protocolVersion"`
		Capabilities    map[string]interface{} `json:"capabilities"`
		Instructions    string                 `json:"instructions"`
		Tools           []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

func decodeRPC(t *testing.T, response *httptest.ResponseRecorder) rpcResult {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	var result rpcResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON-RPC response: %v; body: %s", err, response.Body.String())
	}
	return result
}

func toolCall(name string, arguments interface{}) string {
	encoded, _ := json.Marshal(arguments)
	return `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` + name + `","arguments":` + string(encoded) + `}}`
}

func readDocumentCall(path string) string {
	return toolCall("read_document", map[string]string{"path": path})
}

// documentText returns the Markdown block of a read_document result.
func documentText(result rpcResult) string {
	if len(result.Result.Content) != 2 {
		return ""
	}
	return result.Result.Content[1].Text
}

func TestMCPRequiresAuthentication(t *testing.T) {
	wiki := newTestWiki(t)

	response := wiki.post(t, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", response.Code)
	}
	if !strings.HasPrefix(response.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", response.Header().Get("WWW-Authenticate"))
	}

	unknown, _, _ := auth.GeneratePersonalToken()
	if response := wiki.post(t, unknown, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token status = %d, want 401", response.Code)
	}
}

func TestMCPRejectsRevokedTokensImmediately(t *testing.T) {
	wiki := newTestWiki(t)
	if response := wiki.post(t, wiki.viewerToken, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); response.Code != http.StatusOK {
		t.Fatalf("status before revocation = %d, want 200", response.Code)
	}

	wiki.cfg.Users[0].Tokens = nil
	auth.SetConfig(wiki.cfg)
	if response := wiki.post(t, wiki.viewerToken, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("status after revocation = %d, want 401", response.Code)
	}
}

func TestMCPAcceptsLoginSessionCookie(t *testing.T) {
	wiki := newTestWiki(t)
	recorder := httptest.NewRecorder()
	if err := auth.CreateSession(recorder, "alice", config.RoleViewer, nil, false, wiki.cfg); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(readDocumentCall("guides/intro")))
	for _, cookie := range recorder.Result().Cookies() {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	wiki.server.ServeHTTP(response, request)

	result := decodeRPC(t, response)
	if result.Result.IsError || documentText(result) != "# Intro" {
		t.Fatalf("result = %+v, want the intro document", result.Result)
	}
}

func TestMCPRejectsCrossOriginRequests(t *testing.T) {
	wiki := newTestWiki(t)
	request := httptest.NewRequest(http.MethodPost, "http://wiki.example.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	request.Header.Set("Authorization", "Bearer "+wiki.viewerToken)
	request.Header.Set("Origin", "https://attacker.example.net")
	response := httptest.NewRecorder()
	wiki.server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "http://wiki.example.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	request.Header.Set("Authorization", "Bearer "+wiki.viewerToken)
	request.Header.Set("Origin", "http://wiki.example.com")
	response = httptest.NewRecorder()
	wiki.server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("same-origin status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
}

func TestMCPProtocolLifecycle(t *testing.T) {
	wiki := newTestWiki(t)

	initialize := decodeRPC(t, wiki.post(t, wiki.viewerToken,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	if initialize.Error != nil || initialize.Result.ProtocolVersion != "2025-03-26" {
		t.Fatalf("initialize = %+v, want the requested supported version", initialize)
	}
	if _, ok := initialize.Result.Capabilities["tools"]; !ok {
		t.Fatalf("capabilities = %+v, want tools", initialize.Result.Capabilities)
	}
	if !strings.Contains(initialize.Result.Instructions, "search_documents") {
		t.Fatalf("instructions = %q, want the tool workflow", initialize.Result.Instructions)
	}

	unknownVersion := decodeRPC(t, wiki.post(t, wiki.viewerToken,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`))
	if unknownVersion.Result.ProtocolVersion != LatestProtocolVersion {
		t.Fatalf("protocolVersion = %q, want %q", unknownVersion.Result.ProtocolVersion, LatestProtocolVersion)
	}

	if response := wiki.post(t, wiki.viewerToken, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); response.Code != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", response.Code)
	}

	list := decodeRPC(t, wiki.post(t, wiki.viewerToken, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
	var names []string
	for _, tool := range list.Result.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "search_documents,list_documents,read_document,add_comment" {
		t.Fatalf("tools = %v, want the viewer tools in registry order", names)
	}

	unknown := decodeRPC(t, wiki.post(t, wiki.viewerToken, `{"jsonrpc":"2.0","id":4,"method":"resources/list"}`))
	if unknown.Error == nil || unknown.Error.Code != codeMethodNotFound {
		t.Fatalf("unknown method error = %+v, want %d", unknown.Error, codeMethodNotFound)
	}

	parse := decodeRPC(t, wiki.post(t, wiki.viewerToken, `{not json`))
	if parse.Error == nil || parse.Error.Code != codeParseError {
		t.Fatalf("parse error = %+v, want %d", parse.Error, codeParseError)
	}

	unknownTool := decodeRPC(t, wiki.post(t, wiki.viewerToken, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"delete_everything"}}`))
	if unknownTool.Error == nil || unknownTool.Error.Code != codeInvalidParams {
		t.Fatalf("unknown tool error = %+v, want %d", unknownTool.Error, codeInvalidParams)
	}

	request := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+wiki.viewerToken)
	response := httptest.NewRecorder()
	wiki.server.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":6,"method":"ping"}`))
	request.Header.Set("Authorization", "Bearer "+wiki.viewerToken)
	request.Header.Set("MCP-Protocol-Version", "1999-01-01")
	response = httptest.NewRecorder()
	wiki.server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unsupported protocol header status = %d, want 400", response.Code)
	}
}

func TestReadDocumentAppliesAccessRules(t *testing.T) {
	wiki := newTestWiki(t)

	tests := []struct {
		name    string
		token   string
		path    string
		want    string
		isError bool
	}{
		{"public document", wiki.viewerToken, "guides/intro", "# Intro", false},
		{"leading slash", wiki.viewerToken, "/guides/intro", "# Intro", false},
		{"home page", wiki.viewerToken, "", "# Home", false},
		{"home page slash", wiki.viewerToken, "/", "# Home", false},
		{"restricted without group", wiki.viewerToken, "finance/report", "Document not found or access denied", true},
		{"restricted with group", wiki.financeToken, "finance/report", "# Report", false},
		{"missing document", wiki.viewerToken, "guides/missing", "Document not found or access denied", true},
		{"parent traversal", wiki.viewerToken, "../config.yaml", "Invalid document path", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := decodeRPC(t, wiki.post(t, test.token, readDocumentCall(test.path)))
			if result.Error != nil {
				t.Fatalf("protocol error = %+v, want a tool result", result.Error)
			}
			if result.Result.IsError != test.isError {
				t.Fatalf("result = %+v, want isError=%t", result.Result, test.isError)
			}
			if test.isError && (len(result.Result.Content) != 1 || result.Result.Content[0].Text != test.want) {
				t.Fatalf("result = %+v, want error %q", result.Result, test.want)
			}
			if !test.isError && documentText(result) != test.want {
				t.Fatalf("result = %+v, want document %q", result.Result, test.want)
			}
		})
	}
}

func TestToolsRequireAKnownRole(t *testing.T) {
	wiki := newTestWiki(t)
	wiki.cfg.Users[0].Role = ""
	auth.SetConfig(wiki.cfg)

	list := decodeRPC(t, wiki.post(t, wiki.viewerToken, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
	if list.Error != nil || len(list.Result.Tools) != 0 {
		t.Fatalf("tools/list = %+v, want no tools for an unknown role", list)
	}

	calls := []string{
		readDocumentCall("guides/intro"),
		toolCall("search_documents", map[string]string{"query": "intro"}),
		toolCall("list_documents", map[string]string{}),
	}
	for _, call := range calls {
		result := decodeRPC(t, wiki.post(t, wiki.viewerToken, call))
		if !result.Result.IsError || !strings.Contains(result.Result.Content[0].Text, "Insufficient role") {
			t.Fatalf("result = %+v, want an insufficient role error", result.Result)
		}
	}
}
