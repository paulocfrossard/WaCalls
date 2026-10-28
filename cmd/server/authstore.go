package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type authStore struct{ db *sql.DB }

func newAuthStore(ctx context.Context, db *sql.DB) (*authStore, error) {
	if db == nil {
		return nil, errors.New("nil db")
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id            TEXT PRIMARY KEY,
			username      TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'operator',
			created_at    INTEGER NOT NULL,
			last_login    INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name       TEXT NOT NULL,
			key_hash   TEXT NOT NULL,
			key_prefix TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_used  INTEGER,
			revoked    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS sessions_auth (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			revoked    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			ts         INTEGER NOT NULL,
			actor      TEXT NOT NULL,
			action     TEXT NOT NULL,
			resource   TEXT,
			ip         TEXT,
			user_agent TEXT,
			detail     TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_log(actor)`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("authstore: newAuthStore: %w", err)
		}
	}
	return &authStore{db: db}, nil
}

func (s *authStore) insertUser(ctx context.Context, id, username, passwordHash, role string, createdAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, username, passwordHash, role, createdAt)
	if err != nil {
		return fmt.Errorf("authstore: insertUser: %w", err)
	}
	return nil
}

func (s *authStore) getUserByUsername(ctx context.Context, username string) (id, passwordHash, role string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT id, password_hash, role FROM users WHERE username = ?`, username).
		Scan(&id, &passwordHash, &role)
	if err != nil {
		return id, passwordHash, role, fmt.Errorf("authstore: getUserByUsername: %w", err)
	}
	return id, passwordHash, role, nil
}

func (s *authStore) insertApiKey(ctx context.Context, id, userID, name, keyHash, keyPrefix string, createdAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, user_id, name, key_hash, key_prefix, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, userID, name, keyHash, keyPrefix, createdAt)
	if err != nil {
		return fmt.Errorf("authstore: insertApiKey: %w", err)
	}
	return nil
}

func (s *authStore) getApiKeyByHash(ctx context.Context, keyHash string) (id, userID string, revoked bool, err error) {
	var revokedInt int
	err = s.db.QueryRowContext(ctx,
		`SELECT id, user_id, revoked FROM api_keys WHERE key_hash = ?`, keyHash).
		Scan(&id, &userID, &revokedInt)
	if err != nil {
		return "", "", false, fmt.Errorf("authstore: getApiKeyByHash: %w", err)
	}
	return id, userID, revokedInt != 0, nil
}

func (s *authStore) insertAudit(ctx context.Context, ts int64, actor, action, resource, ip, userAgent, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, action, resource, ip, user_agent, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ts, actor, action, resource, ip, userAgent, detail)
	if err != nil {
		return fmt.Errorf("authstore: insertAudit: %w", err)
	}
	return nil
}
