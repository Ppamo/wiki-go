package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"wiki-go/internal/config"
	"wiki-go/internal/roles"
)

// PersonalTokenPrefix marks a bearer credential as a personal access token
// rather than a session token, so the resolver knows where to look it up.
const PersonalTokenPrefix = "wgo_pat_"

// tokenConfig is an immutable snapshot of the users and their personal access
// tokens. Handlers publish a new snapshot after saving a configuration change.
var tokenConfig atomic.Pointer[config.Config]

// SetConfig registers the configuration used to resolve personal access tokens.
func SetConfig(cfg *config.Config) {
	if cfg == nil {
		tokenConfig.Store(nil)
		return
	}

	// Authentication runs concurrently with configuration updates. Keep an
	// immutable copy here so a request never observes a partially published
	// Users/Tokens slice.
	snapshot := *cfg
	snapshot.Users = append([]config.User(nil), cfg.Users...)
	for i := range snapshot.Users {
		snapshot.Users[i].Groups = append([]string(nil), cfg.Users[i].Groups...)
		snapshot.Users[i].Tokens = append([]config.APIToken(nil), cfg.Users[i].Tokens...)
	}
	tokenConfig.Store(&snapshot)
}

// GeneratePersonalToken returns a new personal access token and the hash to
// store in the config. The plain token must be shown to the user only once.
func GeneratePersonalToken() (token string, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = PersonalTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

// bearerToken returns the credential sent as "Authorization: Bearer <token>".
func bearerToken(r *http.Request) string {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// lookupPersonalToken resolves a personal access token to a session built from
// the owner's current role and groups, so changes made by an admin apply on the
// next request. The token role, when set, caps the owner's role.
func lookupPersonalToken(token string) *Session {
	cfg := tokenConfig.Load()
	if cfg == nil {
		return nil
	}

	hashed := []byte(hashToken(token))
	now := time.Now()
	for _, user := range cfg.Users {
		for _, t := range user.Tokens {
			if subtle.ConstantTimeCompare(hashed, []byte(t.Hash)) != 1 {
				continue
			}
			if t.IsExpired(now) {
				return nil
			}

			role := user.Role
			if t.Role != "" && roles.Rank(t.Role) < roles.Rank(role) {
				role = t.Role
			}
			return &Session{
				Username:     user.Username,
				Role:         role,
				Groups:       user.Groups,
				TokenName:    t.Name,
				CreatedAt:    t.CreatedAt,
				ExpiresAt:    t.ExpiresAt,
				LastAccessed: now,
			}
		}
	}
	return nil
}
