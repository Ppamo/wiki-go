package handlers

import (
	"net/http"
	"strings"
	"sync"
	"wiki-go/internal/auth"
	"wiki-go/internal/config"
	"wiki-go/internal/i18n"
	"wiki-go/internal/logger"
)

var (
	cfg           *config.Config
	configWriteMu sync.RWMutex
)

// ConfigReadMiddleware keeps the shared configuration stable for requests
// that only read it. Configuration-mutating handlers take the write side of
// configWriteMu themselves, so they must not also take a read lock here.
func ConfigReadMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isConfigWriteRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		configWriteMu.RLock()
		defer configWriteMu.RUnlock()
		next.ServeHTTP(w, r)
	})
}

func isConfigWriteRequest(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false
	}

	switch r.URL.Path {
	case "/api/password", "/api/tokens", "/api/users", "/api/settings/wiki", "/api/settings/security", "/api/access-rules":
		return true
	default:
		return strings.HasPrefix(r.URL.Path, "/api/access-rules/")
	}
}

// publishConfig makes a successfully persisted configuration visible to both
// handlers and the personal-token resolver. Callers must hold configWriteMu.
func publishConfig(updated config.Config) {
	*cfg = updated
	auth.SetConfig(cfg)
}

// InitHandlers initializes the handlers with the given configuration
func InitHandlers(config *config.Config) {
	cfg = config

	// Let the auth package resolve personal access tokens from the users
	auth.SetConfig(cfg)

	// Initialize i18n package
	if err := i18n.Initialize(cfg); err != nil {
		logger.Warn("Failed to initialize i18n package: %v", err)
	}

	// Initialise IP-based ban list for login attempts
	InitLoginBan(cfg)

	// Routes are now managed in the routes package
}

// We don't need this anymore since main.go handles the routing
// func handlePage(w http.ResponseWriter, r *http.Request) {
//     PageHandler(w, r, cfg)
// }
