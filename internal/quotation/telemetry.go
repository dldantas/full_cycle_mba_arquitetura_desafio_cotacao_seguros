package quotation

import (
	"context"
	"log"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrica de negocio do cache (docs/sad.md, secao 6). No Prometheus vira
// quotation_cache_requests_total{result="hit|miss|error", partner, tenant_id}, e o hit rate e
//
//	sum(rate(quotation_cache_requests_total{result="hit"}[5m])) / sum(rate(quotation_cache_requests_total[5m]))
//
// tenant_id e o unico identificador que entra: CPF, placa e quote_id nunca.
const (
	cacheHit   = "hit"
	cacheMiss  = "miss"
	cacheError = "error"
)

type metrics struct {
	cache metric.Int64Counter
}

func newMetrics() *metrics {
	meter := otel.Meter("github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/quotation")
	counter, err := meter.Int64Counter("quotation.cache.requests",
		metric.WithDescription("Leituras do cache de cotacao por parceira: hit, miss ou error (Redis indisponivel, tratado como miss)"))
	if err != nil {
		log.Printf("quotation metrics: %v", err)
	}
	return &metrics{cache: counter}
}

func (m *metrics) cacheResult(ctx context.Context, tenant, partner, result string) {
	if m.cache == nil {
		return
	}
	m.cache.Add(ctx, 1, metric.WithAttributes(
		attribute.String("result", result),
		attribute.String("partner", partner),
		attribute.String("tenant_id", tenant),
	))
}
