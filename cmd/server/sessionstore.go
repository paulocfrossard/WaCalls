package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
)

type sessionRow struct {
	ID         string
	Name       string
	JID        string
	EverPaired bool
}

type sessionStore struct{ db *sql.DB }

func newSessionStore(ctx context.Context, db *sql.DB) (*sessionStore, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS sessions (
		id   TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		jid  TEXT
	)`)
	if err != nil {
		return nil, err
	}

	// Idempotent migration: add ever_paired column if missing, then backfill
	// from existing jid values.
	hasEverPaired, err := columnExists(ctx, db, "sessions", "ever_paired")
	if err != nil {
		return nil, err
	}
	if !hasEverPaired {
		if _, err := db.ExecContext(ctx, `ALTER TABLE sessions ADD COLUMN ever_paired INTEGER NOT NULL DEFAULT 0`); err != nil {
			return nil, err
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET ever_paired = 1 WHERE jid IS NOT NULL AND jid != ''`); err != nil {
		return nil, err
	}
	return &sessionStore{db: db}, nil
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	// Whitelist known (table, column) pairs instead of interpolating names.
	if table != "sessions" || column != "ever_paired" {
		return false, nil
	}
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func newSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *sessionStore) list(ctx context.Context) ([]sessionRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, COALESCE(jid, ''), COALESCE(ever_paired, 0) FROM sessions ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		var v int
		if err := rows.Scan(&r.ID, &r.Name, &r.JID, &v); err != nil {
			return nil, err
		}
		r.EverPaired = (v != 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *sessionStore) insert(ctx context.Context, id, name string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, name, jid) VALUES (?, ?, NULL)`, id, name)
	return err
}

func (s *sessionStore) setJID(ctx context.Context, id, jid string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET jid = ?, ever_paired = CASE WHEN ? != '' THEN 1 ELSE ever_paired END WHERE id = ?`, jid, jid, id)
	return err
}

func (s *sessionStore) delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}
