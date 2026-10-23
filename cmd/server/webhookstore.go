package main

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

type Webhook struct {
	ID        string
	URL       string
	Secret    string
	Events    []string // empty = subscribe to all events
	CreatedAt time.Time
}

type webhookStore struct{ db *sql.DB }

func newWebhookStore(ctx context.Context, db *sql.DB) (*webhookStore, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS webhooks (
		id         TEXT PRIMARY KEY,
		url        TEXT NOT NULL,
		secret     TEXT NOT NULL,
		events     TEXT,
		created_at DATETIME NOT NULL
	)`)
	if err != nil {
		return nil, err
	}
	return &webhookStore{db: db}, nil
}

func (s *webhookStore) Create(ctx context.Context, url, secret string, events []string) (string, error) {
	id := newSessionID()
	eventsCSV := strings.Join(events, ",")
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webhooks (id, url, secret, events, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, url, secret, eventsCSV, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *webhookStore) List(ctx context.Context) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, url, secret, COALESCE(events, ''), created_at FROM webhooks ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		var w Webhook
		var eventsCSV, createdAt string
		if err := rows.Scan(&w.ID, &w.URL, &w.Secret, &eventsCSV, &createdAt); err != nil {
			return nil, err
		}
		w.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		if eventsCSV != "" {
			for _, tok := range strings.Split(eventsCSV, ",") {
				tok = strings.TrimSpace(tok)
				if tok != "" {
					w.Events = append(w.Events, tok)
				}
			}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *webhookStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetForEvent returns webhooks subscribed to eventType. A webhook with an
// empty Events list matches every event.
func (s *webhookStore) GetForEvent(ctx context.Context, eventType string) ([]Webhook, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Webhook
	for _, w := range all {
		if len(w.Events) == 0 {
			out = append(out, w)
			continue
		}
		for _, e := range w.Events {
			if e == eventType {
				out = append(out, w)
				break
			}
		}
	}
	return out, nil
}
