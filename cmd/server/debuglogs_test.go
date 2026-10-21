package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"wacalls/internal/voip/logbuf"
)

// TestDebugLogsEndpointContract drives the real mux and pins the exact JSON
// contract the front-end logs page consumes:
//
//	GET /api/debug/logs?since=<seq>&limit=<n>
//	→ {"events":[{"seq","ts","source","level","msg","fields"}],"nextSeq":N}
func TestDebugLogsEndpointContract(t *testing.T) {
	logbuf.Default().Reset()
	logbuf.SetEnabled(true)
	defer logbuf.Default().Reset()

	logbuf.LogEvent("uplink-mic", logbuf.LevelDebug, "browser pcm captured", map[string]any{
		"call_id": "C1", "len": 1600, "rms": 0.1, "peak": 0.3,
	})

	// routes() only wires closures; a bare server is enough for this handler.
	ts := httptest.NewServer((&server{}).routes())
	defer ts.Close()

	get := func(rawQuery string) (int, struct {
		Events  []map[string]any `json:"events"`
		NextSeq int64            `json:"nextSeq"`
	}) {
		t.Helper()
		var body struct {
			Events  []map[string]any `json:"events"`
			NextSeq int64            `json:"nextSeq"`
		}
		resp, err := http.Get(ts.URL + "/api/debug/logs" + rawQuery)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("invalid JSON %q: %v", raw, err)
		}
		return resp.StatusCode, body
	}

	code, body := get("?since=0&limit=100")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body.NextSeq != 1 {
		t.Fatalf("nextSeq = %d, want 1", body.NextSeq)
	}
	if len(body.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(body.Events))
	}
	ev := body.Events[0]
	for _, key := range []string{"seq", "ts", "source", "level", "msg", "fields"} {
		if _, ok := ev[key]; !ok {
			t.Fatalf("event missing %q: %v", key, ev)
		}
	}
	if ev["seq"] != float64(1) || ev["source"] != "uplink-mic" || ev["level"] != "debug" {
		t.Fatalf("unexpected event: %v", ev)
	}
	fields, ok := ev["fields"].(map[string]any)
	if !ok || fields["call_id"] != "C1" || fields["len"] != float64(1600) {
		t.Fatalf("unexpected fields: %v", ev["fields"])
	}

	// Tail: since=nextSeq must return an empty page without regressing.
	code, body = get("?since=1&limit=100")
	if code != http.StatusOK || len(body.Events) != 0 || body.NextSeq != 1 {
		t.Fatalf("tail page: code=%d events=%d nextSeq=%d", code, len(body.Events), body.NextSeq)
	}

	// limit is capped at 500 and defaults sanely; bogus input must not 500.
	for _, q := range []string{"?limit=999999", "?limit=-5", "?since=abc", "?since=-1", "?limit="} {
		code, _ := get(q)
		if code != http.StatusOK {
			t.Fatalf("query %q → status %d, want 200", q, code)
		}
	}
}

// TestDebugLogsRespectsDisabledFlag: with -debug-log=false the ring stores
// nothing, so the page sees an empty feed instead of telemetry.
func TestDebugLogsRespectsDisabledFlag(t *testing.T) {
	logbuf.Default().Reset()
	defer func() {
		logbuf.SetEnabled(true)
		logbuf.Default().Reset()
	}()

	logbuf.SetEnabled(false)
	logbuf.LogEvent("uplink-mic", logbuf.LevelDebug, "must be dropped", nil)
	if logbuf.Default().Len() != 0 {
		t.Fatal("disabled ring must stay empty")
	}
}
