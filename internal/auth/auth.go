// Package auth issues and verifies the credentials the collector uses.
//
// Secrets are never stored in a form that can be read back: enrollment codes
// and device tokens are kept as SHA-256 digests, passwords as argon2id hashes.
// A database dump therefore does not yield a working credential.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/argon2"

	"github.com/christianparpart/agentic-stats/internal/store"
	"github.com/christianparpart/agentic-stats/internal/wire"
)

// ErrInvalidCredential reports a rejected token, code or password. It is
// deliberately indistinguishable between "not found" and "wrong", so that the
// API cannot be used to enumerate valid identifiers.
var ErrInvalidCredential = errors.New("auth: invalid credential")

// Role names a user's privilege level.
type Role string

const (
	// RoleUser can see only their own data.
	RoleUser Role = "user"
	// RoleAdmin can additionally see instance health, but never another
	// user's content.
	RoleAdmin Role = "admin"
)

// Device is an authenticated machine.
type Device struct {
	ID     string
	UserID string
	// Hostname is carried for logging and attribution.
	Hostname string
}

// Service issues and verifies credentials.
type Service struct {
	db *store.DB
}

// NewService returns a Service backed by db.
func NewService(db *store.DB) (*Service, error) {
	if db == nil {
		return nil, errors.New("auth: a database is required")
	}
	return &Service{db: db}, nil
}

// hashSecret digests a bearer secret for storage and lookup.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// newSecret returns a fresh, high-entropy secret in URL-safe form.
func newSecret(nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// argon2 parameters, chosen for an interactive login on modest hardware.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword returns an encoded argon2id hash, salt included.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches an encoded hash.
func VerifyPassword(encoded, password string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 || parts[0] != "argon2id" {
		return ErrInvalidCredential
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrInvalidCredential
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return ErrInvalidCredential
	}
	got := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrInvalidCredential
	}
	return nil
}

// CreateUser registers a user and returns its id.
func (s *Service) CreateUser(ctx context.Context, email, password string, role Role) (string, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	var id string
	err = s.db.InAuthTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO users (email, password_hash, role)
			VALUES ($1, $2, $3)
			RETURNING id::text`, email, hash, string(role)).Scan(&id)
	})
	if err != nil {
		return "", fmt.Errorf("auth: create user: %w", err)
	}
	return id, nil
}

// CreateEnrollmentCode mints a short-lived code a machine redeems once.
func (s *Service) CreateEnrollmentCode(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	code, err := newSecret(16)
	if err != nil {
		return "", err
	}
	err = s.db.InTenantTx(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		_, xerr := tx.Exec(ctx, `
			INSERT INTO enrollment_codes (code_hash, user_id, expires_at)
			VALUES ($1, $2::uuid, now() + $3::interval)`,
			hashSecret(code), userID, ttl.String())
		return xerr
	})
	if err != nil {
		return "", fmt.Errorf("auth: create enrollment code: %w", err)
	}
	return code, nil
}

// RedeemEnrollment exchanges a valid code for a durable device token.
//
// The code is single-use: redemption is recorded in the same transaction that
// issues the token, so a replayed code cannot mint a second one.
func (s *Service) RedeemEnrollment(ctx context.Context, code string, dev wire.Device) (string, error) {
	var userID string
	err := s.db.InAuthTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			UPDATE enrollment_codes
			   SET redeemed_at = now()
			 WHERE code_hash = $1
			   AND redeemed_at IS NULL
			   AND expires_at > now()
			RETURNING user_id::text`, hashSecret(code)).Scan(&userID)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrInvalidCredential
		}
		return "", fmt.Errorf("auth: redeem enrollment: %w", err)
	}

	token, err := newSecret(32)
	if err != nil {
		return "", err
	}
	err = s.db.InTenantTx(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var deviceID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO devices (user_id, hostname, os, arch, timezone, agent_version)
			VALUES ($1::uuid, $2, $3, $4, $5, $6)
			ON CONFLICT (user_id, hostname, os, arch) DO UPDATE
			    SET last_seen = now(), timezone = EXCLUDED.timezone,
			        agent_version = EXCLUDED.agent_version
			RETURNING id::text`,
			userID, dev.Hostname, dev.OS, dev.Arch, dev.Timezone, dev.AgentVersion,
		).Scan(&deviceID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO device_tokens (token_hash, device_id, user_id)
			VALUES ($1, $2::uuid, $3::uuid)`, hashSecret(token), deviceID, userID)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("auth: issue device token: %w", err)
	}
	return token, nil
}

// AuthenticateDevice resolves a bearer token to its device.
//
// Two steps, and the split is forced by the schema rather than incidental.
// device_tokens is a credential table, readable before a tenant exists --
// establishing the tenant is precisely what this lookup is for. devices is a
// payload table under FORCE row-level security, so it can only be read once
// app.user_id is set. Joining the two in one statement would silently return
// nothing.
func (s *Service) AuthenticateDevice(ctx context.Context, token string) (Device, error) {
	var dev Device
	err := s.db.InAuthTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			UPDATE device_tokens
			   SET last_used = now()
			 WHERE token_hash = $1
			   AND revoked_at IS NULL
			RETURNING device_id::text, user_id::text`,
			hashSecret(token)).Scan(&dev.ID, &dev.UserID)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Device{}, ErrInvalidCredential
		}
		return Device{}, fmt.Errorf("auth: authenticate device: %w", err)
	}

	err = s.db.InTenantTx(ctx, dev.UserID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT hostname FROM devices WHERE id = $1::uuid`, dev.ID).Scan(&dev.Hostname)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The token outlived its device; treat it as revoked.
			return Device{}, ErrInvalidCredential
		}
		return Device{}, fmt.Errorf("auth: load device: %w", err)
	}
	return dev, nil
}
