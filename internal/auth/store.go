// SPDX-License-Identifier: Apache-2.0
package auth

import (
	"errors"
	"sync"

	"golang.org/x/crypto/bcrypt"

	"github.com/deepmav/pocketpilot-cloud/internal/token"
)

type User struct {
	ID           string
	Username     string
	PasswordHash []byte
	Role         token.Role
}

// MemoryStore is a PoC in-memory user table. Replace with SQLite/Postgres
// when persistence is needed.
type MemoryStore struct {
	mu    sync.RWMutex
	users map[string]*User // keyed by username
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{users: map[string]*User{}}
}

func (s *MemoryStore) Add(username, password string, role token.Role) (*User, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	u := &User{
		ID:           username,
		Username:     username,
		PasswordHash: h,
		Role:         role,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.users[username]; exists {
		return nil, errors.New("user exists")
	}
	s.users[username] = u
	return u, nil
}

var ErrInvalidCredentials = errors.New("invalid credentials")

func (s *MemoryStore) Authenticate(username, password string) (*User, error) {
	s.mu.RLock()
	u, ok := s.users[username]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword(u.PasswordHash, []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

func (s *MemoryStore) SeedDev() error {
	if _, err := s.Add("pilot1", "pilot1-dev", token.RoleUser); err != nil {
		return err
	}
	if _, err := s.Add("drone-42", "drone-42-dev", token.RoleDrone); err != nil {
		return err
	}
	return nil
}
