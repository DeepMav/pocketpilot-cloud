// SPDX-License-Identifier: Apache-2.0
package auth

import (
	"encoding/json"
	"net/http"

	"github.com/deepmav/pocketpilot-cloud/internal/token"
)

type HTTPHandler struct {
	store  *MemoryStore
	issuer *token.Mint
}

func NewHTTPHandler(store *MemoryStore, issuer *token.Mint) *HTTPHandler {
	return &HTTPHandler{store: store, issuer: issuer}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	AccessToken string     `json:"access_token"`
	ExpiresAt   int64      `json:"expires_at"`
	Subject     string     `json:"sub"`
	Role        token.Role `json:"role"`
}

func (h *HTTPHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	u, err := h.store.Authenticate(req.Username, req.Password)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "username or password incorrect")
		return
	}
	tok, exp, err := h.issuer.Issue(u.ID, u.Role)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issue_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(loginResponse{
		AccessToken: tok,
		ExpiresAt:   exp.Unix(),
		Subject:     u.ID,
		Role:        u.Role,
	})
}

func (h *HTTPHandler) Me(w http.ResponseWriter, r *http.Request) {
	c := token.ClaimsFrom(r.Context())
	if c == nil {
		writeErr(w, http.StatusUnauthorized, "no_claims", "no claims in context")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sub":  c.Subject,
		"role": c.Role,
		"exp":  c.ExpiresAt.Unix(),
	})
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}
