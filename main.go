// Command model-router serves an OpenAI-compatible passthrough proxy.
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

	"model-router/internal/config"
	"model-router/internal/proxy"
	"model-router/internal/version"
)

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON configuration file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Get())
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	app, err := proxy.NewServer(*configPath, cfg)
	if err != nil {
		log.Fatalf("init proxy: %v", err)
	}

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("model-router %s listening on %s with %d instance(s)", version.Get(), cfg.Listen, len(cfg.Instances))
		for _, inst := range cfg.Instances {
			log.Printf("instance: protocol=%s base_url=%s models=%v roles=%v timeout=%s",
				inst.Protocol, inst.BaseURL, inst.Models, inst.Roles, inst.Timeout.Duration())
		}
		if cfg.AdminPassword == "" {
			log.Printf("admin UI disabled (admin_password is empty)")
		} else {
			log.Printf("admin UI enabled at /admin for user %q", cfg.AdminUser)
		}
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	case <-ctx.Done():
		stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}
