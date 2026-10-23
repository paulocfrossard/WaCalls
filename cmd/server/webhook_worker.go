package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// EventDispatcher receives broker events for outbound webhook delivery.
// Dispatch must be non-blocking: it only enqueues work.
type EventDispatcher interface {
	Dispatch(eventType string, payload any)
}

// webhookEvents are the broker event types eligible for outbound delivery.
// Session/QR/list events are internal-only and never leave via webhooks.
// incoming-claimed is deliberately excluded (UI-only claim signal).
var webhookEvents = map[string]bool{
	"incoming":    true,
	"call-status": true,
	"call-ended":  true,
}

// WebhookEvent is the outbound JSON envelope delivered to external receivers.
type WebhookEvent struct {
	Event      string    `json:"event"`
	Timestamp  time.Time `json:"timestamp"`
	DeliveryID string    `json:"deliveryId"`
	Data       any       `json:"data"`
}

type webhookJob struct {
	event WebhookEvent
	body  []byte // marshaled envelope, reused for both HMAC and POST body
}

type webhookWorker struct {
	store   *webhookStore
	log     *slog.Logger
	client  *http.Client
	jobs    chan webhookJob
	backoff []time.Duration

	// allowHosts, when non-empty, restricts delivery targets to these exact
	// hostnames (case-insensitive, any port). Enforced at delivery time as
	// well as at registration.
	allowHosts []string

	// lookupTimeout bounds GetForEvent so a stalled SQLite pool (MaxOpenConns=1
	// shared with whatsmeow) cannot pin a worker forever. Slightly above the
	// DB busy_timeout (10s) to avoid dropping healthy jobs under transient
	// contention.
	lookupTimeout time.Duration

	// jobTimeout caps how long one worker slot is occupied by a single job
	// (all hooks including retry chains). Prevents one dead receiver from
	// holding a worker for the full multi-minute worst case.
	jobTimeout time.Duration

	// sem is a worker-global concurrency cap on simultaneous hook goroutines.
	sem chan struct{}

	shutdownOnce sync.Once
}

const (
	maxHooksPerJob       = 8
	defaultLookupTimeout = 12 * time.Second
	defaultJobTimeout    = 25 * time.Second
)

func newWebhookWorker(store *webhookStore, log *slog.Logger, allowHosts []string) *webhookWorker {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 5
	return &webhookWorker{
		store: store,
		log:   log,
		client: &http.Client{
			Timeout: 5 * time.Second,
			// Never follow redirects: a 301/302 rewrites POST→GET without a
			// body (false success) and would forward the signature header to
			// an arbitrary target. 3xx is returned as-is and treated as a
			// failed attempt.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: tr,
		},
		jobs: make(chan webhookJob, 64),
		// Exponential backoff between the 3 total attempts (1s then 5s).
		backoff:       []time.Duration{1 * time.Second, 5 * time.Second},
		allowHosts:    allowHosts,
		lookupTimeout: defaultLookupTimeout,
		jobTimeout:    defaultJobTimeout,
		sem:           make(chan struct{}, maxHooksPerJob),
	}
}

// Dispatch implements EventDispatcher. Non-blocking: drops the event (with an
// error log) when the buffer is full so a slow or dead receiver can never
// stall live calls.
func (w *webhookWorker) Dispatch(eventType string, payload any) {
	if !webhookEvents[eventType] {
		return
	}
	env := WebhookEvent{
		Event:      eventType,
		Timestamp:  time.Now().UTC(),
		DeliveryID: newSessionID(),
		Data:       webhookData(eventType, payload),
	}
	body, err := json.Marshal(env)
	if err != nil {
		w.log.Error("webhook marshal failed", "event", eventType, "err", err)
		return
	}
	select {
	case w.jobs <- webhookJob{event: env, body: body}:
	default:
		w.log.Error("webhook buffer full, dropping event", "event", eventType)
	}
}

func (w *webhookWorker) Start(ctx context.Context, workers int) {
	for i := 0; i < workers; i++ {
		go w.loop(ctx)
	}
}

func (w *webhookWorker) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			w.logPendingOnShutdown()
			return
		case job := <-w.jobs:
			w.deliver(ctx, job)
		}
	}
}

// logPendingOnShutdown reports how many buffered events were abandoned when
// the process shuts down. Guarded by shutdownOnce so only the first exiting
// worker logs.
func (w *webhookWorker) logPendingOnShutdown() {
	w.shutdownOnce.Do(func() {
		if n := len(w.jobs); n > 0 {
			w.log.Warn("webhook worker exiting with undelivered events", "pending", n)
		}
	})
}

func (w *webhookWorker) deliver(ctx context.Context, job webhookJob) {
	lookupCtx, cancel := context.WithTimeout(ctx, w.lookupTimeout)
	hooks, err := w.store.GetForEvent(lookupCtx, job.event.Event)
	cancel()
	if err != nil {
		w.log.Error("webhook lookup failed", "event", job.event.Event, "err", err)
		return
	}
	if len(hooks) == 0 {
		return
	}

	// Bound the whole job so one dead receiver cannot pin this worker for the
	// full multi-attempt chain (was ~21s sequential worst case).
	jobCtx, cancelJob := context.WithTimeout(ctx, w.jobTimeout)
	defer cancelJob()

	var wg sync.WaitGroup
	for _, h := range hooks {
		h := h
		wg.Add(1)
		// Acquire a slot on the worker-global semaphore, respecting job
		// cancellation so we never leak goroutines on shutdown.
		select {
		case w.sem <- struct{}{}:
		case <-jobCtx.Done():
			wg.Done()
			continue
		}
		go func() {
			defer wg.Done()
			defer func() { <-w.sem }()
			w.deliverOne(jobCtx, h, job)
		}()
	}
	wg.Wait()
}

func (w *webhookWorker) deliverOne(ctx context.Context, h Webhook, job webhookJob) {
	// Defense in depth: re-check URL policy at delivery time (protects rows
	// created before a -webhook-allow-hosts flag was set).
	if err := validateWebhookURL(h.URL, w.allowHosts); err != nil {
		w.log.Error("webhook delivery skipped by URL policy",
			"url", redactURL(h.URL), "event", job.event.Event, "err", err)
		return
	}

	sig := signPayload(job.body, h.Secret)
	safeURL := redactURL(h.URL)
	for attempt := 0; attempt <= len(w.backoff); attempt++ {
		if attempt > 0 {
			wait := w.backoff[attempt-1]
			w.log.Info("webhook retry scheduled",
				"url", safeURL, "event", job.event.Event, "attempt", attempt+1, "wait", wait)
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		status, err := w.post(ctx, h, job, sig)
		if err == nil && status >= 200 && status < 300 {
			w.log.Info("webhook delivered",
				"url", safeURL, "event", job.event.Event, "status", status, "attempt", attempt+1)
			return
		}
		// Retry only transient failures: network errors (status 0), 5xx,
		// 408 Request Timeout, 429 Too Many Requests. Permanent 4xx will never
		// succeed — give up immediately.
		if err == nil && status >= 400 && status < 500 && status != 408 && status != 429 {
			w.log.Error("webhook permanent failure, not retrying",
				"url", safeURL, "event", job.event.Event, "status", status)
			return
		}
		w.log.Warn("webhook delivery failed",
			"url", safeURL, "event", job.event.Event, "status", status, "attempt", attempt+1, "err", err)
	}
	w.log.Error("webhook giving up after retries",
		"url", safeURL, "event", job.event.Event, "attempts", len(w.backoff)+1)
}

func (w *webhookWorker) post(ctx context.Context, h Webhook, job webhookJob, sig string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(job.body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "WaCalls-Webhook/1.0")
	req.Header.Set("X-WaCalls-Event", job.event.Event)
	req.Header.Set("X-WaCalls-Signature", "sha256="+sig)
	req.Header.Set("X-WaCalls-Delivery-Id", job.event.DeliveryID)
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	// Drain a bounded amount of the body so the connection can be reused
	// (keep-alive); without this, net/http will not return it to the idle pool.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()
	return resp.StatusCode, nil
}

// sanitizeErr strips the URL from *url.Error so credentials in the URL
// (query string or userinfo) never reach the logs.
func sanitizeErr(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %v", ue.Op, ue.Err)
	}
	return err
}

func signPayload(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// validateWebhookURL enforces scheme, host presence, length, and the optional
// hostname allowlist. Used both at registration and at delivery time.
func validateWebhookURL(rawURL string, allowHosts []string) error {
	if len(rawURL) == 0 || len(rawURL) > 2048 {
		return errInvalidURL("url must be 1..2048 characters")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return errInvalidURL("invalid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errInvalidURL("scheme must be http or https")
	}
	if u.Hostname() == "" {
		return errInvalidURL("url must include a host")
	}
	if len(allowHosts) == 0 {
		return nil
	}
	host := u.Hostname()
	for _, allowed := range allowHosts {
		if strings.EqualFold(host, allowed) {
			return nil
		}
	}
	return errInvalidURL("host not in webhook allowlist")
}

type urlError string

func (e urlError) Error() string { return string(e) }

func errInvalidURL(msg string) error { return urlError(msg) }

// redactURL returns scheme://host/path with query and fragment stripped so
// credentials embedded in query strings never reach the logs.
func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "[invalid-url]"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// webhookData maps the raw broker event into the stable outbound contract:
// sessionId, callId (broker uses "id"), peer, direction, plus per-event extras.
func webhookData(eventType string, payload any) any {
	m, ok := payload.(map[string]any)
	if !ok {
		return payload
	}
	data := map[string]any{}
	if v, ok := m["sessionId"]; ok {
		data["sessionId"] = v
	}
	if v, ok := m["id"]; ok {
		data["callId"] = v
	}
	if v, ok := m["peer"]; ok {
		data["peer"] = v
	}
	if v, ok := m["direction"]; ok {
		data["direction"] = v
	}
	switch eventType {
	case "incoming":
		if _, exists := data["direction"]; !exists {
			data["direction"] = "inbound"
		}
		if v, ok := m["offeredAt"]; ok {
			data["offeredAt"] = v
		}
	case "call-status":
		if v, ok := m["status"]; ok {
			data["status"] = v
		}
		if v, ok := m["owner"]; ok {
			data["owner"] = v
		}
		if v, ok := m["startedAt"]; ok {
			data["startedAt"] = v
		}
	case "call-ended":
		if v, ok := m["reason"]; ok {
			data["endReason"] = v
		}
		if v, ok := m["endedAt"]; ok {
			data["endedAt"] = v
		}
		if v, ok := m["owner"]; ok {
			data["owner"] = v
		}
	}
	return data
}
