package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSessionStoreRoundtrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sessions_test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	st, err := newSessionStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	id := newSessionID()
	if len(id) != 32 {
		t.Fatalf("session id should be 32 hex chars, got %d", len(id))
	}
	if err := st.insert(ctx, id, "Account A"); err != nil {
		t.Fatal(err)
	}

	rows, err := st.list(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id || rows[0].Name != "Account A" || rows[0].JID != "" {
		t.Fatalf("unexpected rows after insert: %+v", rows)
	}

	if err := st.setJID(ctx, id, "5511999999999:1@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.list(ctx)
	if rows[0].JID != "5511999999999:1@s.whatsapp.net" {
		t.Fatalf("jid not persisted: %+v", rows[0])
	}

	if err := st.delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.list(ctx)
	if len(rows) != 0 {
		t.Fatalf("expected empty after delete, got %+v", rows)
	}
}

func TestSessionStoreMigrationIdempotent(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "mig_test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	st1, err := newSessionStore(ctx, db)
	if err != nil {
		t.Fatalf("first newSessionStore: %v", err)
	}
	_ = st1

	st2, err := newSessionStore(ctx, db)
	if err != nil {
		t.Fatalf("second newSessionStore (idempotent): %v", err)
	}
	_ = st2

	ok, err := columnExists(ctx, db, "sessions", "ever_paired")
	if err != nil {
		t.Fatalf("columnExists: %v", err)
	}
	if !ok {
		t.Fatal("ever_paired column must exist after migration")
	}

	// Backfill: insert a row with jid set but ever_paired left at default 0,
	// reopen the store, and assert the migration backfills ever_paired=1.
	id := newSessionID()
	if _, err := db.ExecContext(ctx, `INSERT INTO sessions (id, name, jid) VALUES (?, ?, ?)`, id, "backfill", "15551234567@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	// Force ever_paired back to 0 to simulate a pre-migration row.
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET ever_paired = 0 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	st3, err := newSessionStore(ctx, db)
	if err != nil {
		t.Fatalf("reopen newSessionStore: %v", err)
	}
	rows, err := st3.list(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.ID == id {
			found = true
			if !r.EverPaired {
				t.Fatalf("backfill did not set ever_paired=1 for row with jid, got %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("backfill row not found")
	}
}
