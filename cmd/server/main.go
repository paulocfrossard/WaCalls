package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"wacalls/internal/voip/logbuf"
	"wacalls/internal/voip/media"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "wacalls.db", "SQLite session database path")
	staticDir := flag.String("static", "client/dist", "static client directory (optional)")
	debug := flag.Bool("debug", false, "verbose logging")
	debugLog := flag.Bool("debug-log", true, "emit audio diagnostic events to the in-memory ring served at GET /api/debug/logs")
	maxCalls := flag.Int("max-calls-per-session", 8, "max concurrent calls per session (0 = unlimited)")
	authSRTP := flag.Bool("srtp-auth", true, "verify SRTP HMAC tag on receive (legacy: false)")
	webhookAllowHosts := flag.String("webhook-allow-hosts", "", "comma-separated hostnames webhooks may target (empty = any host)")
	flag.Parse()
	media.SetAuthVerify(*authSRTP)
	logbuf.SetEnabled(*debugLog)

	var allowHosts []string
	for _, h := range strings.Split(*webhookAllowHosts, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		// Strip IPv6 brackets so entries compare equal to url.Hostname().
		h = strings.TrimPrefix(h, "[")
		h = strings.TrimSuffix(h, "]")
		allowHosts = append(allowHosts, h)
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Watches WritePCM timestamps and warns (source=dup-detector) when two
	// writes for the same call land <50ms apart = duplicated audio path.
	go logbuf.StartDupDetector(ctx)

	verCtx, verCancel := context.WithTimeout(ctx, 10*time.Second)
	if v, verr := whatsmeow.GetLatestVersion(verCtx, nil); verr != nil {
		log.Warn("could not refresh whatsapp web version; using bundled default", "err", verr)
	} else {
		store.SetWAVersion(*v)
		log.Info("whatsapp web version set", "version", v.String())
	}
	verCancel()

	srv, err := newServer(ctx, *dbPath, *staticDir, *maxCalls, allowHosts, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	defer srv.sessions.disconnectAll()

	if err := srv.sessions.Restore(ctx); err != nil {
		log.Error("session restore failed", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{Addr: *addr, Handler: srv.routes()}
	go func() {
		log.Info("HTTP server listening", "addr", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server error", "err", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}
