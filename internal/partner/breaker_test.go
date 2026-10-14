package partner

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/resilience"
)

// flakySequence sao as 60 primeiras respostas da partner-flaky com os defaults do compose (semente
// 20260729, 40% de falha), 'X' = 503 e '.' = 200, copiadas de docs/smoke-test-factibilidade.md
// (secao 2.2), que as mediu no container e as prendeu em cmd/partner-mock/feasibility_test.go.
// A rajada de 9 falhas esta nas sequencias 49 a 57.
const flakySequence = "...XX.XXX..X..X.X...XXX.....X..X.....XXXXX......XX" + "XXXXXXX.X."

// sadBreaker sao os parametros do docs/sad.md, secao 4.1 (e os defaults de internal/platform/config.go).
var sadBreaker = resilience.Settings{ConsecutiveFailures: 6, OpenFor: 10 * time.Second, HalfOpenMaxCalls: 2}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// replayFlaky responde na ordem da flakySequence e conta quantas chamadas de fato chegaram.
func replayFlaky(t *testing.T) (platform.Partner, *atomic.Int64) {
	t.Helper()
	var received atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seq := received.Add(1)
		if flakySequence[seq-1] == 'X' {
			http.Error(w, `{"error":"partner unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"quote_id":"q","premium_cents":100000,"currency":"BRL"}`))
	}))
	t.Cleanup(server.Close)
	return platform.Partner{Name: "partner-flaky", BaseURL: server.URL}, &received
}

func TestBreakerOpensInsideTheBurstAndStopsCallingThePartner(t *testing.T) {
	p, received := replayFlaky(t)
	clock := &fakeClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	client := NewClient(WithBreakers([]platform.Partner{p}, sadBreaker, resilience.WithClock(clock.now)))

	openedAt := 0
	for attempt := 1; attempt <= 60; attempt++ {
		_, err := client.Quote(context.Background(), p, map[string]string{})
		var failure *Error
		if errors.As(err, &failure) && failure.Kind == OutcomeShortCircuited && openedAt == 0 {
			openedAt = attempt - 1
		}
	}

	// Nenhuma rajada de 6 antes da sequencia 49; a de 9 (49 a 57) atinge 6 na sequencia 54.
	if openedAt != 54 {
		t.Fatalf("breaker opened after call %d, the SAD predicts call 54 (6th failure of the 49-57 burst)", openedAt)
	}
	if got := received.Load(); got != 54 {
		t.Fatalf("the partner received %d calls; with the circuit open from call 54 it must stop at 54", got)
	}
	if client.State(p.Name) != resilience.StateOpen {
		t.Fatalf("state %s, want open", client.State(p.Name))
	}

	// Passado o OpenFor, o meio aberto deixa a sonda sair: e a sequencia 55, que e 503, e reabre.
	clock.t = clock.t.Add(sadBreaker.OpenFor)
	if client.State(p.Name) != resilience.StateHalfOpen {
		t.Fatalf("state %s after OpenFor, want half_open", client.State(p.Name))
	}
	if _, err := client.Quote(context.Background(), p, map[string]string{}); err == nil {
		t.Fatal("sequence 55 is a 503 and was answered as success")
	}
	if received.Load() != 55 || client.State(p.Name) != resilience.StateOpen {
		t.Fatalf("failed probe: %d calls received, state %s; want 55 and open", received.Load(), client.State(p.Name))
	}
}

func TestTimeoutCountsAsFailureForTheBreaker(t *testing.T) {
	// A parceira que afunda: nunca responde erro, so nao responde. O handler espera o cliente desistir.
	var received atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received.Add(1)
		_, _ = io.Copy(io.Discard, r.Body) // como o mock: le o corpo, e so assim o servidor percebe a desistencia
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	p := platform.Partner{Name: "partner-degrading", BaseURL: server.URL}

	client := NewClient(WithTimeout(20*time.Millisecond), WithBreakers([]platform.Partner{p}, sadBreaker))
	for i := 0; i < sadBreaker.ConsecutiveFailures; i++ {
		_, err := client.Quote(context.Background(), p, map[string]string{})
		var failure *Error
		if !errors.As(err, &failure) || failure.Kind != OutcomeTimeout {
			t.Fatalf("call %d: error %v, want a timeout", i+1, err)
		}
	}

	if client.State(p.Name) != resilience.StateOpen {
		t.Fatalf("%d timeouts in a row left the breaker %s; a timeout must count as failure",
			sadBreaker.ConsecutiveFailures, client.State(p.Name))
	}
	_, err := client.Quote(context.Background(), p, map[string]string{})
	var failure *Error
	if !errors.As(err, &failure) || failure.Kind != OutcomeShortCircuited {
		t.Fatalf("open circuit answered %v, want short_circuited", err)
	}
	if received.Load() != int64(sadBreaker.ConsecutiveFailures) {
		t.Fatalf("partner received %d calls, want %d: the open circuit must not call it",
			received.Load(), sadBreaker.ConsecutiveFailures)
	}
}

func TestCallerCancellationDoesNotOpenTheBreaker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	p := platform.Partner{Name: "partner-slow", BaseURL: server.URL}
	client := NewClient(WithBreakers([]platform.Partner{p}, sadBreaker))

	for i := 0; i < 3*sadBreaker.ConsecutiveFailures; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // a corretora desistiu antes da resposta
		_, _ = client.Quote(ctx, p, map[string]string{})
	}
	if client.State(p.Name) != resilience.StateClosed {
		t.Fatalf("broker cancellations opened the breaker (%s): they say nothing about the partner", client.State(p.Name))
	}
}
