package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/quotation"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/resilience"
)

func main() {
	cfg, err := platform.LoadConfig(platform.Environment())
	if err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	ctx, stopListening := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopListening()

	stopTelemetry, err := platform.StartTelemetry(context.Background(), cfg.Telemetry)
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}

	// A fatia do docs/sad.md: timeout e um breaker por parceira na fronteira (secao 4.1), cache por
	// parceira com a corretora na chave (4.2), resposta parcial como fallback (4.3) e as tres parceiras
	// em paralelo (4.4). Os numeros vem de cfg.Resilience, cujos defaults sao os do SAD.
	res := cfg.Resilience
	client := partner.NewClient(
		partner.WithTimeout(res.PartnerTimeout),
		partner.WithBreakers(cfg.Partners, resilience.Settings{
			ConsecutiveFailures: res.BreakerFailures,
			OpenFor:             res.BreakerOpenFor,
			HalfOpenMaxCalls:    res.BreakerHalfOpenCalls,
		}),
	)
	quoteCache := cache.NewRedis(res.RedisAddr)
	defer func() { _ = quoteCache.Close() }()

	api := quotation.NewAPI(
		quotation.NewService(cfg.Partners, client,
			quotation.WithCache(quoteCache, res.CacheTTL),
			quotation.WithPartialResponse(),
			quotation.WithParallelCalls(),
		),
		cfg.Tenants,
	)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           platform.InstrumentHandler(api.Routes(), cfg.Telemetry.ServiceName),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Print("quotation-api: signal received, shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("quotation-api: forced shutdown: %v", err)
		}
		if err := stopTelemetry(shutdown); err != nil {
			log.Printf("quotation-api: telemetry shutdown: %v", err)
		}
	}()

	log.Printf("quotation-api listening on %s | partners=%d brokers=%v | timeout=%s breaker=%d failures/%s open/%d half-open | cache ttl=%s redis=%s",
		server.Addr, len(cfg.Partners), cfg.Tenants, res.PartnerTimeout, res.BreakerFailures,
		res.BreakerOpenFor, res.BreakerHalfOpenCalls, res.CacheTTL, res.RedisAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server stopped: %v", err)
	}
}
