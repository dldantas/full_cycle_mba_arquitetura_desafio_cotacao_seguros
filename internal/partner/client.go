package partner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/resilience"
)

type Quote struct {
	Partner         string `json:"partner"`
	QuoteID         string `json:"quote_id"`
	PremiumCents    int64  `json:"premium_cents"`
	Currency        string `json:"currency"`
	CoverageCents   int64  `json:"coverage_cents"`
	ValidForSeconds int64  `json:"valid_for_seconds"`
}

// Client e a fronteira com as parceiras. Sem opcoes ele se comporta como o starter (sem timeout, sem
// breaker); cmd/quotation-api/main.go liga as duas protecoes com os valores do docs/sad.md, secao 4.1.
type Client struct {
	http     *http.Client
	breakers map[string]*resilience.Breaker
	metrics  *metrics
}

type Option func(*Client)

// WithTimeout poe teto na espera por uma parceira (PARTNER_TIMEOUT, 2s no SAD). O estouro conta como
// falha para o breaker: e assim que a partner-degrading, que afunda mas nunca devolve erro, vira sinal.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// WithBreakers cria um breaker por parceira. Escopo por parceira, nao global nem por corretora: a
// instabilidade e da seguradora, e um breaker global derrubaria as tres por causa de uma.
func WithBreakers(partners []platform.Partner, settings resilience.Settings, opts ...resilience.Option) Option {
	return func(c *Client) {
		c.breakers = make(map[string]*resilience.Breaker, len(partners))
		for _, p := range partners {
			c.breakers[p.Name] = resilience.New(p.Name, settings,
				append([]resilience.Option{resilience.WithObserver(c.metrics.transition)}, opts...)...)
		}
	}
}

func NewClient(opts ...Option) *Client {
	c := &Client{
		http:    &http.Client{Transport: platform.InstrumentTransport(http.DefaultTransport)},
		metrics: newMetrics(),
	}
	for _, opt := range opts {
		opt(c)
	}
	c.metrics.observeStates(c.breakers)
	return c
}

// State expoe o estado do breaker de uma parceira (StateClosed quando nao ha breaker).
func (c *Client) State(partner string) resilience.State {
	if b, ok := c.breakers[partner]; ok {
		return b.State()
	}
	return resilience.StateClosed
}

const responseLimit = 1 << 20

func (c *Client) Quote(ctx context.Context, p platform.Partner, request any) (Quote, error) {
	span := trace.SpanFromContext(ctx)

	done := func(resilience.Outcome) {}
	if b, ok := c.breakers[p.Name]; ok {
		state := b.State()
		admitted, err := b.Allow()
		if err != nil {
			// Aberto quer dizer que a chamada nao sai: nenhum span HTTP de saida, nenhuma consulta paga.
			span.SetAttributes(
				attribute.Bool("quotation.circuit.short_circuited", true),
				attribute.String("quotation.circuit.state", b.State().String()),
			)
			span.AddEvent("circuit breaker open: partner not called")
			c.metrics.record(ctx, p.Name, OutcomeShortCircuited, 0)
			return Quote{}, &Error{Partner: p.Name, Kind: OutcomeShortCircuited, Reason: err.Error()}
		}
		span.SetAttributes(attribute.String("quotation.circuit.state", state.String()))
		done = admitted
	}

	started := time.Now()
	quote, err := c.call(ctx, p, request)
	outcome := OutcomeSuccess
	if err != nil {
		outcome = err.Kind
	}
	done(breakerOutcome(outcome))
	c.metrics.record(ctx, p.Name, outcome, time.Since(started))
	span.SetAttributes(attribute.String("quotation.partner.outcome", outcome))

	if err != nil {
		return Quote{}, err
	}
	return quote, nil
}

func (c *Client) call(ctx context.Context, p platform.Partner, request any) (Quote, *Error) {
	body, err := json.Marshal(request)
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Kind: OutcomeInvalid, Reason: fmt.Sprintf("invalid request: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/quotes", bytes.NewReader(body))
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Kind: OutcomeInvalid, Reason: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	response, err := c.http.Do(req)
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Kind: classify(ctx, err), Reason: err.Error()}
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, responseLimit))
		kind := OutcomeError
		if response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests &&
			response.StatusCode != http.StatusRequestTimeout {
			kind = OutcomeRejected
		}
		return Quote{}, &Error{
			Partner: p.Name,
			Status:  response.StatusCode,
			Kind:    kind,
			Reason:  fmt.Sprintf("partner replied %d", response.StatusCode),
		}
	}

	var quote Quote
	if err := json.NewDecoder(io.LimitReader(response.Body, responseLimit)).Decode(&quote); err != nil {
		return Quote{}, &Error{Partner: p.Name, Kind: classify(ctx, err), Reason: fmt.Sprintf("unreadable response: %v", err)}
	}

	quote.Partner = p.Name
	return quote, nil
}

// Valores do atributo outcome da metrica quotation.partner.duration e do campo Kind de Error.
const (
	OutcomeSuccess        = "success"
	OutcomeError          = "error"           // 5xx, 429, 408, transporte, resposta ilegivel: falha
	OutcomeTimeout        = "timeout"         // estourou PARTNER_TIMEOUT: falha
	OutcomeShortCircuited = "short_circuited" // breaker aberto: a parceira nao foi chamada
	OutcomeCanceled       = "canceled"        // a corretora desistiu: nao diz nada da parceira
	OutcomeRejected       = "rejected"        // 4xx: o pedido era o problema, nao a parceira
	OutcomeInvalid        = "invalid_request" // nem chegou a sair
)

func breakerOutcome(outcome string) resilience.Outcome {
	switch outcome {
	case OutcomeSuccess:
		return resilience.Success
	case OutcomeError, OutcomeTimeout:
		return resilience.Failure
	}
	return resilience.Ignored
}

func classify(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return OutcomeCanceled
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return OutcomeTimeout
	}
	return OutcomeError
}

type Error struct {
	Partner string
	Status  int
	Kind    string
	Reason  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("partner %s: %s", e.Partner, e.Reason)
}
