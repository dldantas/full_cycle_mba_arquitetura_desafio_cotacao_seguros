package resilience

import (
	"errors"
	"testing"
	"time"
)

// clock e o relogio de mentira: o tempo so anda quando o teste manda. Nenhum sleep.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

var sadSettings = Settings{ConsecutiveFailures: 6, OpenFor: 10 * time.Second, HalfOpenMaxCalls: 2}

func newTestBreaker(t *testing.T) (*Breaker, *clock, *[]Transition) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	var seen []Transition
	b := New("partner-flaky", sadSettings, WithClock(c.now), WithObserver(func(tr Transition) { seen = append(seen, tr) }))
	return b, c, &seen
}

func call(t *testing.T, b *Breaker, outcome Outcome) {
	t.Helper()
	done, err := b.Allow()
	if err != nil {
		t.Fatalf("call refused in state %s: %v", b.State(), err)
	}
	done(outcome)
}

func TestBreakerWalksTheThreeStates(t *testing.T) {
	b, c, seen := newTestBreaker(t)

	for i := 1; i <= 5; i++ {
		call(t, b, Failure)
	}
	if b.State() != StateClosed {
		t.Fatalf("opened after 5 failures, the SAD says 6")
	}

	call(t, b, Failure)
	if b.State() != StateOpen {
		t.Fatalf("state %s after 6 consecutive failures, want open", b.State())
	}

	if _, err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("open circuit let the call through (err=%v)", err)
	}

	c.advance(9999 * time.Millisecond)
	if b.State() != StateOpen {
		t.Fatalf("left open before OpenFor")
	}
	c.advance(time.Millisecond)
	if b.State() != StateHalfOpen {
		t.Fatalf("state %s after OpenFor, want half_open", b.State())
	}

	first, err := b.Allow()
	if err != nil {
		t.Fatalf("half-open refused the first probe: %v", err)
	}
	second, err := b.Allow()
	if err != nil {
		t.Fatalf("half-open refused the second probe: %v", err)
	}
	if _, err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("half-open admitted a third call, HalfOpenMaxCalls is 2")
	}

	first(Success)
	if b.State() != StateHalfOpen {
		t.Fatalf("closed after a single probe success, want %d", sadSettings.HalfOpenMaxCalls)
	}
	second(Success)
	if b.State() != StateClosed {
		t.Fatalf("state %s after two probe successes, want closed", b.State())
	}

	want := []string{"closed->open", "open->half_open", "half_open->closed"}
	if got := names(*seen); !equal(got, want) {
		t.Fatalf("transitions %v, want %v", got, want)
	}
}

func TestBreakerHalfOpenFailureReopens(t *testing.T) {
	b, c, seen := newTestBreaker(t)
	for i := 0; i < 6; i++ {
		call(t, b, Failure)
	}
	c.advance(sadSettings.OpenFor)

	call(t, b, Failure)
	if b.State() != StateOpen {
		t.Fatalf("state %s after a failed probe, want open", b.State())
	}
	// O prazo recomeca a contar da reabertura, nao da primeira abertura.
	c.advance(sadSettings.OpenFor - time.Millisecond)
	if b.State() != StateOpen {
		t.Fatalf("reopened circuit did not restart OpenFor")
	}

	want := []string{"closed->open", "open->half_open", "half_open->open"}
	if got := names(*seen); !equal(got, want) {
		t.Fatalf("transitions %v, want %v", got, want)
	}
}

func TestBreakerSuccessResetsTheConsecutiveCount(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	for round := 0; round < 10; round++ {
		for i := 0; i < 5; i++ {
			call(t, b, Failure)
		}
		call(t, b, Success)
	}
	if b.State() != StateClosed {
		t.Fatalf("50 failures never 6 in a row opened the circuit")
	}
}

func TestBreakerIgnoredOutcomeNeitherOpensNorCloses(t *testing.T) {
	b, c, _ := newTestBreaker(t)
	for i := 0; i < 20; i++ {
		call(t, b, Ignored)
	}
	if b.State() != StateClosed {
		t.Fatalf("cancelled calls opened the circuit")
	}

	for i := 0; i < 6; i++ {
		call(t, b, Failure)
	}
	c.advance(sadSettings.OpenFor)
	call(t, b, Ignored) // a sonda foi cancelada pela corretora: devolve a vaga, nao fecha
	call(t, b, Success)
	if b.State() != StateHalfOpen {
		t.Fatalf("state %s: an ignored probe counted as success", b.State())
	}
	call(t, b, Success)
	if b.State() != StateClosed {
		t.Fatalf("state %s, want closed", b.State())
	}
}

func TestBreakerDiscardsResultsFromAPreviousState(t *testing.T) {
	b, c, _ := newTestBreaker(t)
	slow, err := b.Allow() // admitida com o circuito fechado
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		call(t, b, Failure)
	}
	c.advance(sadSettings.OpenFor)
	_ = b.State() // meio aberto

	slow(Success) // termina agora: nao pode contar como sonda
	call(t, b, Success)
	if b.State() != StateHalfOpen {
		t.Fatalf("a result from the closed state was counted in half-open")
	}
}

func names(ts []Transition) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.From.String()+"->"+t.To.String())
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
