package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/example/redfish-gpu-exporter/internal/cache"
	"github.com/example/redfish-gpu-exporter/internal/collector"
	"github.com/example/redfish-gpu-exporter/internal/config"
	"github.com/example/redfish-gpu-exporter/internal/metrics"
)

func main() {
	configPath := flag.String("config.file", "configs/exporter.yml", "exporter configuration")
	listen := flag.String("web.listen-address", ":9610", "HTTP listen address")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil { slog.Error("load configuration", "error", err); os.Exit(1) }
	c := collector.New(cfg, cache.New(cfg.CacheTTL))
	h := metrics.New(c)
	mux := http.NewServeMux()
	mux.Handle("/metrics", h.Self())
	mux.HandleFunc("/redfish", h.Redfish)
	mux.HandleFunc("/-/healthy", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/-/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	slog.Info("redfish exporter started", "address", *listen, "cache_ttl", cfg.CacheTTL)
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed { slog.Error("server failed", "error", err); os.Exit(1) }
}
