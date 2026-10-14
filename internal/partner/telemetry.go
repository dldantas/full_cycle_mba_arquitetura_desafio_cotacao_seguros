package partner

import (
	"context"
	"log"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/resilience"
)

// Metricas de negocio da fronteira (docs/sad.md, secao 6). Atributos: so o nome da parceira e o
// resultado. Nada de CPF, placa ou quote_id: alem de dado pessoal fora de lugar, seria cardinalidade
// sem teto.
//
// No Prometheus (via Collector) os nomes viram:
//
//	quotation_partner_duration_seconds_{bucket,count,sum}   latencia por parceira e resultado
//	quotation_partner_circuit_state                         0 fechado, 1 meio aberto, 2 aberto
//	quotation_partner_circuit_transitions_total             transicoes, com from e to
type metrics struct {
	duration    metric.Float64Histogram
	transitions metric.Int64Counter
	state       metric.Int64ObservableGauge
	meter       metric.Meter
}

// Limites em segundos alinhados aos numeros do SAD: 2s e o PARTNER_TIMEOUT, 1,7s o teto da partner-slow.
var durationBuckets = []float64{0.01, 0.05, 0.1, 0.15, 0.2, 0.3, 0.5, 0.75, 1, 1.25, 1.5, 1.75, 2, 2.5, 5}

func newMetrics() *metrics {
	meter := otel.Meter("github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner")
	m := &metrics{meter: meter}

	var err error
	if m.duration, err = meter.Float64Histogram("quotation.partner.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Tempo para obter a cotacao de uma parceira, por resultado (success, error, timeout, short_circuited, canceled)"),
		metric.WithExplicitBucketBoundaries(durationBuckets...)); err != nil {
		log.Printf("partner metrics: %v", err)
	}
	if m.transitions, err = meter.Int64Counter("quotation.partner.circuit.transitions",
		metric.WithDescription("Transicoes de estado do circuit breaker, por parceira, com estado de origem e destino")); err != nil {
		log.Printf("partner metrics: %v", err)
	}
	return m
}

func (m *metrics) record(ctx context.Context, partner, outcome string, elapsed time.Duration) {
	if m.duration == nil {
		return
	}
	m.duration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(
		attribute.String("partner", partner),
		attribute.String("outcome", outcome),
	))
}

func (m *metrics) transition(t resilience.Transition) {
	log.Printf("circuit breaker %s: %s -> %s", t.Name, t.From, t.To)
	if m.transitions == nil {
		return
	}
	m.transitions.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("partner", t.Name),
		attribute.String("from", t.From.String()),
		attribute.String("to", t.To.String()),
	))
}

// observeStates publica o estado atual de cada breaker a cada coleta (10s, internal/platform/telemetry.go).
func (m *metrics) observeStates(breakers map[string]*resilience.Breaker) {
	if len(breakers) == 0 {
		return
	}
	var err error
	m.state, err = m.meter.Int64ObservableGauge("quotation.partner.circuit.state",
		metric.WithDescription("Estado atual do circuit breaker por parceira: 0 fechado, 1 meio aberto, 2 aberto"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for name, b := range breakers {
				o.Observe(int64(b.State()), metric.WithAttributes(attribute.String("partner", name)))
			}
			return nil
		}))
	if err != nil {
		log.Printf("partner metrics: %v", err)
	}
}
