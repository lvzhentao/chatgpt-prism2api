package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"prism-2api/internal/adapter"
	siteadapter "prism-2api/internal/adapter/prism"
	"prism-2api/internal/api"
	"prism-2api/internal/auth"
	"prism-2api/internal/config"
	"prism-2api/internal/debugtrace"
)

var version = "0.1.0"

func main() {
	showVersion := flag.Bool("version", false, "show version")
	cfg := config.ParseFlags()
	if *showVersion {
		fmt.Printf("%s %s\n", siteadapter.DisplayName, version)
		return
	}

	site := siteadapter.New(siteadapter.Config{
		APIBaseURL:    cfg.APIBaseURL,
		WebsiteURL:    cfg.WebsiteURL,
		ClientVersion: cfg.ClientVersion,
		ClientType:    cfg.ClientType,
		Mock:          cfg.MockMode,
	})
	adapter.Bind(site)
	auth.ExchangeHook = site.ExchangeCredential
	auth.LoginURLHook = site.LoginURL
	auth.PollHook = site.PollLogin

	server, err := api.NewServer(cfg)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}
	if cfg.DebugDir != "" {
		debugtrace.Configure(cfg.DebugDir)
		log.Printf("debug JSONL tracing enabled dir=%s", cfg.DebugDir)
	}
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: server.Handler()}

	go func() {
		scheme := "http"
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			scheme = "https"
		}
		log.Printf("%s %s listening on %s://%s (endpoint=%s mock=%v db=%v)", site.Name(), version, scheme, cfg.ListenAddr, cfg.APIBaseURL, cfg.MockMode, cfg.DatabaseURL != "")
		var serveErr error
		if scheme == "https" {
			serveErr = srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			serveErr = srv.ListenAndServe()
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Fatalf("http server: %v", serveErr)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")
	server.Shutdown() // 先停后台循环并刷脏（pool/clientkeys），再停 HTTP
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
