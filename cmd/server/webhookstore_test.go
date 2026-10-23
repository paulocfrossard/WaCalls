package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestWebhookStoreRoundtrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "webhooks_test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	st, err := newWebhookStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	id, err := st.Create(ctx, "https://example.com/hook", "s3cret", []string{"incoming", "call-ended"})
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 32 {
		t.Fatalf("webhook id should be 32 hex chars, got %d", len(id))
	}

	list, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id || list[0].URL != "https://example.com/hook" || list[0].Secret != "s3cret" {
		t.Fatalf("unexpected list after create: %+v", list)
	}
	if len(list[0].Events) != 2 || list[0].Events[0] != "incoming" || list[0].Events[1] != "call-ended" {
		t.Fatalf("events not persisted: %+v", list[0].Events)
	}

	// GetForEvent: matching event, non-matching event, empty events = all.
	incoming, err := st.GetForEvent(ctx, "incoming")
	if err != nil || len(incoming) != 1 {
		t.Fatalf("expected 1 for incoming, got %d err=%v", len(incoming), err)
	}
	other, err := st.GetForEvent(ctx, "session-qr")
	if err != nil || len(other) != 0 {
		t.Fatalf("expected 0 for session-qr, got %d err=%v", len(other), err)
	}

	id2, err := st.Create(ctx, "https://example.com/all", "sec2", nil)
	if err != nil {
		t.Fatal(err)
	}
	// call-status matches only the empty-events webhook (first is incoming/call-ended only).
	status, err := st.GetForEvent(ctx, "call-status")
	if err != nil || len(status) != 1 || status[0].ID != id2 {
		t.Fatalf("expected 1 (empty events = match all), got %d err=%v", len(status), err)
	}
	// incoming matches both: explicit subscription + empty-events catch-all.
	incoming2, err := st.GetForEvent(ctx, "incoming")
	if err != nil || len(incoming2) != 2 {
		t.Fatalf("expected 2 for incoming, got %d err=%v", len(incoming2), err)
	}

	if err := st.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	list, _ = st.List(ctx)
	if len(list) != 1 {
		t.Fatalf("expected 1 after delete, got %d", len(list))
	}
	if err := st.Delete(ctx, "nonexistent"); err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows for missing delete, got %v", err)
	}
}
