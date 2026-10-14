package quotation

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type Quoter interface {
	Quote(ctx context.Context, p platform.Partner, request any) (partner.Quote, error)
}

// Cache e o que o servico precisa do cache (internal/cache.Redis implementa).
type Cache interface {
	Get(ctx context.Context, key string) (cache.Entry, error)
	Set(ctx context.Context, key string, entry cache.Entry, ttl time.Duration) error
}

// Service agrega as parceiras. Sem opcoes ele e o starter: em serie e tudo ou nada (os testes que
// vieram com o repositorio fixam esse comportamento). cmd/quotation-api/main.go liga as tres opcoes
// que o docs/sad.md decide: cache por parceira, resposta parcial e chamadas em paralelo.
type Service struct {
	partners []platform.Partner
	quoter   Quoter
	cache    Cache
	cacheTTL time.Duration
	partial  bool
	parallel bool
	now      func() time.Time
	metrics  *metrics
}

type Option func(*Service)

// WithCache le o cache antes de chamar a parceira e grava cada cotacao comprada com o TTL do SAD
// (CACHE_TTL, 1h). Falha do Redis vira miss: o cache e atalho, nao dependencia.
func WithCache(c Cache, ttl time.Duration) Option {
	return func(s *Service) { s.cache, s.cacheTTL = c, ttl }
}

// WithPartialResponse e o fallback: a cotacao sai com as parceiras que responderam (ou estavam no
// cache) e diz quais faltaram. So falha inteira quando nenhuma parceira tem cotacao.
func WithPartialResponse() Option {
	return func(s *Service) { s.partial = true }
}

// WithParallelCalls consulta as parceiras ao mesmo tempo: a latencia passa a ser a da mais lenta, nao
// a soma das tres (bonus do desafio, docs/sad.md, secao 4.4).
func WithParallelCalls() Option {
	return func(s *Service) { s.parallel = true }
}

// WithClock injeta o relogio usado para quoted_at e age_seconds (testes deterministicos).
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

func NewService(partners []platform.Partner, quoter Quoter, opts ...Option) *Service {
	s := &Service{partners: partners, quoter: quoter, now: time.Now, metrics: newMetrics()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var tracer = otel.Tracer("github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/quotation")

type outcome struct {
	offer Offer
	err   error
}

func (s *Service) Quote(ctx context.Context, tenant string, request Request) (Response, error) {
	start := time.Now()
	forPartner := partnerRequest{Broker: tenant, Request: request}

	outcomes := make([]outcome, len(s.partners))
	if s.parallel {
		var wg sync.WaitGroup
		for i, p := range s.partners {
			wg.Add(1)
			go func() {
				defer wg.Done()
				outcomes[i] = s.quotePartner(ctx, tenant, request, forPartner, p)
			}()
		}
		wg.Wait()
	} else {
		for i, p := range s.partners {
			outcomes[i] = s.quotePartner(ctx, tenant, request, forPartner, p)
			if outcomes[i].err != nil && !s.partial {
				return Response{}, outcomes[i].err
			}
		}
	}

	response := Response{TenantID: tenant, Quotes: make([]Offer, 0, len(s.partners))}
	var firstErr error
	cacheHits := 0
	for i, o := range outcomes {
		if o.err != nil {
			if !s.partial {
				return Response{}, o.err
			}
			if firstErr == nil {
				firstErr = o.err
			}
			response.Unavailable = append(response.Unavailable, Unavailable{
				Partner: s.partners[i].Name,
				Reason:  reason(o.err),
			})
			continue
		}
		if o.offer.Source == SourceCache {
			cacheHits++
		}
		response.Quotes = append(response.Quotes, o.offer)
	}

	// Nenhuma parceira e nenhum cache: nao ha o que mostrar, e premio inventado e proibido. Volta o
	// 502 de sempre, com o nome da parceira (handler.go).
	if len(response.Quotes) == 0 {
		return Response{}, firstErr
	}

	sort.SliceStable(response.Quotes, func(i, j int) bool {
		return response.Quotes[i].PremiumCents < response.Quotes[j].PremiumCents
	})
	response.Degraded = len(response.Unavailable) > 0
	response.ElapsedMs = time.Since(start).Milliseconds()

	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("quotation.tenant_id", tenant),
		attribute.Bool("quotation.degraded", response.Degraded),
		attribute.Int("quotation.cache_hits", cacheHits),
		attribute.Int("quotation.quotes", len(response.Quotes)),
	)
	return response, nil
}

// quotePartner obtem a cotacao de uma parceira: do cache, se houver, senao da parceira (que pode
// estar com o circuito aberto, e ai nem e chamada). Um span por parceira, com as duas marcacoes do
// desafio: quotation.cache.hit e quotation.circuit.short_circuited (este, posto pelo partner.Client).
func (s *Service) quotePartner(ctx context.Context, tenant string, request Request, forPartner partnerRequest, p platform.Partner) outcome {
	ctx, span := tracer.Start(ctx, "quotation.partner",
		trace.WithAttributes(attribute.String("quotation.partner", p.Name)))
	defer span.End()

	key, writeBack := "", false
	if s.cache != nil {
		key = CacheKey(tenant, p.Name, request)
		entry, err := s.cache.Get(ctx, key)
		switch {
		case err == nil:
			age := s.now().Sub(entry.QuotedAt)
			s.metrics.cacheResult(ctx, tenant, p.Name, cacheHit)
			span.SetAttributes(
				attribute.Bool("quotation.cache.hit", true),
				attribute.String("quotation.source", SourceCache),
				attribute.Int64("quotation.cache.age_seconds", int64(age.Seconds())),
			)
			return outcome{offer: fromEntry(entry, age)}
		case errors.Is(err, cache.ErrMiss):
			s.metrics.cacheResult(ctx, tenant, p.Name, cacheMiss)
			writeBack = true
		default:
			// Redis fora: segue como miss e nao tenta gravar. Gravar pagaria o timeout do Redis uma
			// segunda vez na mesma cotacao (medido: +0,4 s, docs/evidencias/redis-fora-1-antes-da-correcao.txt).
			s.metrics.cacheResult(ctx, tenant, p.Name, cacheError)
			span.AddEvent("cache unavailable: treated as miss")
		}
		span.SetAttributes(attribute.Bool("quotation.cache.hit", false))
	}

	quote, err := s.quoter.Quote(ctx, p, forPartner)
	if err != nil {
		span.SetAttributes(attribute.String("quotation.unavailable_reason", reason(err)))
		return outcome{err: err}
	}

	quotedAt := s.now()
	span.SetAttributes(attribute.String("quotation.source", SourcePartner))
	if writeBack {
		if err := s.cache.Set(ctx, key, toEntry(quote, quotedAt), s.cacheTTL); err != nil {
			span.AddEvent("cache write failed")
		}
	}
	return outcome{offer: Offer{Quote: quote, Source: SourcePartner, QuotedAt: quotedAt}}
}

func reason(err error) string {
	var failure *partner.Error
	if errors.As(err, &failure) && failure.Kind != "" {
		return failure.Kind
	}
	return partner.OutcomeError
}

func toEntry(q partner.Quote, quotedAt time.Time) cache.Entry {
	return cache.Entry{
		Partner:         q.Partner,
		QuoteID:         q.QuoteID,
		PremiumCents:    q.PremiumCents,
		Currency:        q.Currency,
		CoverageCents:   q.CoverageCents,
		ValidForSeconds: q.ValidForSeconds,
		QuotedAt:        quotedAt,
	}
}

func fromEntry(e cache.Entry, age time.Duration) Offer {
	return Offer{
		Quote: partner.Quote{
			Partner:         e.Partner,
			QuoteID:         e.QuoteID,
			PremiumCents:    e.PremiumCents,
			Currency:        e.Currency,
			CoverageCents:   e.CoverageCents,
			ValidForSeconds: e.ValidForSeconds,
		},
		Source:     SourceCache,
		QuotedAt:   e.QuotedAt,
		AgeSeconds: int64(age.Seconds()),
	}
}
