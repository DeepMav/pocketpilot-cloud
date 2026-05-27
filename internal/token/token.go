// SPDX-License-Identifier: Apache-2.0
package token

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Role string

const (
	RoleUser  Role = "user"
	RoleDrone Role = "drone"
)

const Issuer = "pocketpilot-auth"

type Claims struct {
	Role Role `json:"role"`
	jwt.RegisteredClaims
}

// Mint signs new tokens. Only cmd/auth uses this.
type Mint struct {
	secret    []byte
	accessTTL time.Duration
}

func NewIssuer(secret []byte, accessTTL time.Duration) *Mint {
	return &Mint{secret: secret, accessTTL: accessTTL}
}

func (m *Mint) Issue(subject string, role Role) (signed string, expiresAt time.Time, err error) {
	now := time.Now()
	exp := now.Add(m.accessTTL)
	c := Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			Issuer:    Issuer,
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	s, err := tok.SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign: %w", err)
	}
	return s, exp, nil
}

// Verifier validates tokens. cmd/auth and cmd/signal both hold one.
//
// Migration target: RS256 + JWKS so cmd/signal can run with public key only,
// keeping the signing key in cmd/auth alone.
type Verifier struct {
	secret []byte
}

func NewVerifier(secret []byte) *Verifier {
	return &Verifier{secret: secret}
}

func (v *Verifier) Verify(raw string) (*Claims, error) {
	parsed, err := jwt.ParseWithClaims(raw, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return v.secret, nil
	}, jwt.WithIssuer(Issuer))
	if err != nil {
		return nil, err
	}
	c, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return nil, errors.New("invalid token")
	}
	return c, nil
}
