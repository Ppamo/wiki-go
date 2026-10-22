package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"time"
	"wiki-go/internal/auth"
	"wiki-go/internal/config"
	"wiki-go/internal/logger"
	"wiki-go/internal/roles"
)

const (
	// maxTokensPerUser keeps the config file small; tokens live in config.yaml.
	maxTokensPerUser = 20
	// maxTokenExpiryDays is the longest expiry accepted for a new token.
	maxTokenExpiryDays = 3650
)

var tokenNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// TokenResponse describes a personal access token without its secret
type TokenResponse struct {
	Name      string     `json:"name"`
	Role      string     `json:"role,omitempty"` // Empty means the owner's role
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Expired   bool       `json:"expired"`
}

// TokenCreateRequest represents the request body for creating a token
type TokenCreateRequest struct {
	Name          string `json:"name"`
	Role          string `json:"role,omitempty"`            // Optional cap, at most the owner's role
	ExpiresInDays int    `json:"expires_in_days,omitempty"` // 0 means the token does not expire
}

// TokensHandler manages the personal access tokens of the current user.
// Tokens can only be managed from a login session, so a leaked token cannot
// be used to mint new ones or to hide itself.
func TokensHandler(w http.ResponseWriter, r *http.Request) {
	session := auth.GetSession(r)
	if session == nil {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized, "")
		return
	}
	if session.IsPersonalToken() {
		sendJSONError(w, "Personal access tokens cannot manage tokens", http.StatusForbidden, "")
		return
	}
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		configWriteMu.Lock()
		defer configWriteMu.Unlock()
	} else if r.Method != http.MethodGet {
		sendJSONError(w, "Method not allowed", http.StatusMethodNotAllowed, "")
		return
	}

	userIndex := -1
	for i, user := range cfg.Users {
		if user.Username == session.Username {
			userIndex = i
			break
		}
	}
	if userIndex < 0 {
		sendJSONError(w, "User not found", http.StatusNotFound, "")
		return
	}

	switch r.Method {
	case http.MethodGet:
		listTokens(w, cfg.Users[userIndex])
	case http.MethodPost:
		createToken(w, r, session, userIndex)
	case http.MethodDelete:
		deleteToken(w, r, session, userIndex)
	}
}

func tokenResponse(token config.APIToken, now time.Time) TokenResponse {
	response := TokenResponse{
		Name:      token.Name,
		Role:      token.Role,
		CreatedAt: token.CreatedAt,
		Expired:   token.IsExpired(now),
	}
	if !token.ExpiresAt.IsZero() {
		expiresAt := token.ExpiresAt
		response.ExpiresAt = &expiresAt
	}
	return response
}

func listTokens(w http.ResponseWriter, user config.User) {
	now := time.Now()
	tokens := make([]TokenResponse, 0, len(user.Tokens))
	for _, token := range user.Tokens {
		tokens = append(tokens, tokenResponse(token, now))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"tokens":  tokens,
	})
}

func createToken(w http.ResponseWriter, r *http.Request, session *auth.Session, userIndex int) {
	var req TokenCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request payload", http.StatusBadRequest, err.Error())
		return
	}
	defer r.Body.Close()

	user := cfg.Users[userIndex]
	if !tokenNamePattern.MatchString(req.Name) {
		sendJSONError(w, "Token name must be 1-64 letters, digits, '.', '_' or '-'", http.StatusBadRequest, "")
		return
	}
	if req.Role != "" {
		if roles.Rank(req.Role) == 0 {
			sendJSONError(w, "Invalid role", http.StatusBadRequest, "")
			return
		}
		if roles.Rank(req.Role) > roles.Rank(user.Role) {
			sendJSONError(w, "Token role cannot exceed your own role", http.StatusForbidden, "")
			return
		}
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > maxTokenExpiryDays {
		sendJSONError(w, "expires_in_days must be between 0 and 3650", http.StatusBadRequest, "")
		return
	}
	if len(user.Tokens) >= maxTokensPerUser {
		sendJSONError(w, "Too many tokens; delete one first", http.StatusConflict, "")
		return
	}
	for _, token := range user.Tokens {
		if token.Name == req.Name {
			sendJSONError(w, "A token with that name already exists", http.StatusConflict, "")
			return
		}
	}

	plain, hash, err := auth.GeneratePersonalToken()
	if err != nil {
		sendJSONError(w, "Failed to generate token", http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	token := config.APIToken{
		Name:      req.Name,
		Hash:      hash,
		Role:      req.Role,
		CreatedAt: now,
	}
	if req.ExpiresInDays > 0 {
		token.ExpiresAt = now.AddDate(0, 0, req.ExpiresInDays)
	}

	// Create a copy of the current config
	updatedConfig := *cfg
	updatedConfig.Users = append([]config.User(nil), cfg.Users...)
	updatedConfig.Users[userIndex].Tokens = append(append([]config.APIToken(nil), user.Tokens...), token)

	// Save the updated config
	if err := saveConfig(config.ConfigFilePath, &updatedConfig); err != nil {
		sendJSONError(w, "Failed to save configuration", http.StatusInternalServerError, err.Error())
		return
	}

	// Update the global config
	publishConfig(updatedConfig)

	logger.Info("User %s created personal access token %s", session.Username, token.Name)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Token created; copy it now, it will not be shown again",
		"token":   plain,
		"details": tokenResponse(token, now),
	})
}

func deleteToken(w http.ResponseWriter, r *http.Request, session *auth.Session, userIndex int) {
	name := r.URL.Query().Get("name")
	if name == "" {
		sendJSONError(w, "Token name is required", http.StatusBadRequest, "")
		return
	}

	user := cfg.Users[userIndex]
	tokens := make([]config.APIToken, 0, len(user.Tokens))
	for _, token := range user.Tokens {
		if token.Name != name {
			tokens = append(tokens, token)
		}
	}
	if len(tokens) == len(user.Tokens) {
		sendJSONError(w, "Token not found", http.StatusNotFound, "")
		return
	}

	// Create a copy of the current config
	updatedConfig := *cfg
	updatedConfig.Users = append([]config.User(nil), cfg.Users...)
	updatedConfig.Users[userIndex].Tokens = tokens

	// Save the updated config
	if err := saveConfig(config.ConfigFilePath, &updatedConfig); err != nil {
		sendJSONError(w, "Failed to save configuration", http.StatusInternalServerError, err.Error())
		return
	}

	// Update the global config
	publishConfig(updatedConfig)

	logger.Info("User %s deleted personal access token %s", session.Username, name)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Token deleted",
	})
}
