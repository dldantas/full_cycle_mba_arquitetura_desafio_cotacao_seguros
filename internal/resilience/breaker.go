// Package resilience guarda o circuit breaker da fronteira com as parceiras (docs/sad.md, secao 4.1).
//
// Um breaker por parceira. Os tres estados sao explicitos e o estado aberto significa que a chamada
// NAO sai: Allow devolve ErrOpen antes de qualquer I/O, e a consulta de R$ 0,04 nao e comprada.
package resilience

import (
	"errors"
	"sync"
	"time"
)

type State int

const (
	StateClosed State = iota
	StateHalfOpen
	StateOpen
)

// String e o valor que vai para os atributos from/to da metrica de transicao.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateHalfOpen:
		return "half_open"
	case StateOpen:
		return "open"
	}
	return "unknown"
}

// Outcome e o que o chamador declara sobre a chamada que o breaker deixou passar.
type Outcome int

const (
	// Success zera a contagem de falhas (fechado) ou conta para fechar (meio aberto).
	Success Outcome = iota
	// Failure e 5xx, erro de transporte, resposta ilegivel e estouro do timeout do cliente.
	Failure
	// Ignored nao diz nada sobre a parceira: a corretora desistiu (contexto cancelado) ou o pedido
	// era invalido. Nao conta para abrir nem para fechar.
	Ignored
)

var ErrOpen = errors.New("circuit breaker open: partner not called")

type Settings struct {
	// ConsecutiveFailures abre o circuito fechado. 6 no SAD: a partner-flaky falha 40% das vezes de
	// forma independente, e 0,4^6 = 0,4% torna a abertura falsa rara, enquanto a rajada de 9 falhas
	// (sequencias 49 a 57) abre na sequencia 54.
	ConsecutiveFailures int
	// OpenFor e quanto tempo o circuito fica aberto antes de deixar uma sonda passar.
	OpenFor time.Duration
	// HalfOpenMaxCalls e quantas chamadas o meio aberto admite; o mesmo numero de sucessos fecha.
	HalfOpenMaxCalls int
}

// Transition e entregue ao observador fora do lock do breaker.
type Transition struct {
	Name string
	From State
	To   State
	At   time.Time
}

type Breaker struct {
	name     string
	settings Settings
	now      func() time.Time
	observe  func(Transition)

	mu                sync.Mutex
	state             State
	generation        uint64
	failures          int
	openedAt          time.Time
	halfOpenAdmitted  int
	halfOpenSucceeded int
}

type Option func(*Breaker)

// WithClock injeta o relogio. E o que torna os testes deterministicos: nenhum sleep para ver o
// circuito sair de aberto para meio aberto.
func WithClock(now func() time.Time) Option {
	return func(b *Breaker) { b.now = now }
}

// WithObserver recebe cada transicao de estado (metrica e log).
func WithObserver(observe func(Transition)) Option {
	return func(b *Breaker) { b.observe = observe }
}

func New(name string, settings Settings, opts ...Option) *Breaker {
	if settings.ConsecutiveFailures < 1 {
		settings.ConsecutiveFailures = 1
	}
	if settings.HalfOpenMaxCalls < 1 {
		settings.HalfOpenMaxCalls = 1
	}
	b := &Breaker{name: name, settings: settings, now: time.Now, observe: func(Transition) {}}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

func (b *Breaker) Name() string { return b.name }

// State le o estado atual, aplicando a passagem de aberto para meio aberto se o prazo venceu.
func (b *Breaker) State() State {
	b.mu.Lock()
	transitions := b.advance(b.now())
	state := b.state
	b.mu.Unlock()
	b.emit(transitions)
	return state
}

// Allow decide se a chamada sai. Com o circuito aberto (ou o meio aberto lotado) devolve ErrOpen e
// a parceira nao e chamada. Quando admite, devolve done, que o chamador TEM que invocar uma vez com
// o resultado da chamada.
func (b *Breaker) Allow() (done func(Outcome), err error) {
	b.mu.Lock()
	transitions := b.advance(b.now())

	switch b.state {
	case StateOpen:
		b.mu.Unlock()
		b.emit(transitions)
		return nil, ErrOpen
	case StateHalfOpen:
		if b.halfOpenAdmitted >= b.settings.HalfOpenMaxCalls {
			b.mu.Unlock()
			b.emit(transitions)
			return nil, ErrOpen
		}
		b.halfOpenAdmitted++
	}

	generation := b.generation
	b.mu.Unlock()
	b.emit(transitions)

	var once sync.Once
	return func(outcome Outcome) {
		once.Do(func() { b.record(generation, outcome) })
	}, nil
}

func (b *Breaker) record(generation uint64, outcome Outcome) {
	b.mu.Lock()
	now := b.now()
	transitions := b.advance(now)

	// Resultado de uma chamada admitida num estado que ja acabou (ex.: uma chamada lenta do fechado
	// que termina depois de o circuito abrir) nao diz nada sobre o estado atual.
	if generation != b.generation {
		b.mu.Unlock()
		b.emit(transitions)
		return
	}

	switch b.state {
	case StateClosed:
		switch outcome {
		case Success:
			b.failures = 0
		case Failure:
			b.failures++
			if b.failures >= b.settings.ConsecutiveFailures {
				transitions = append(transitions, b.moveTo(StateOpen, now))
			}
		}
	case StateHalfOpen:
		switch outcome {
		case Success:
			b.halfOpenSucceeded++
			if b.halfOpenSucceeded >= b.settings.HalfOpenMaxCalls {
				transitions = append(transitions, b.moveTo(StateClosed, now))
			}
		case Failure:
			transitions = append(transitions, b.moveTo(StateOpen, now))
		case Ignored:
			b.halfOpenAdmitted--
		}
	}
	b.mu.Unlock()
	b.emit(transitions)
}

// advance aplica a unica transicao movida pelo tempo: aberto -> meio aberto.
func (b *Breaker) advance(now time.Time) []Transition {
	if b.state == StateOpen && !now.Before(b.openedAt.Add(b.settings.OpenFor)) {
		return []Transition{b.moveTo(StateHalfOpen, now)}
	}
	return nil
}

func (b *Breaker) moveTo(to State, now time.Time) Transition {
	from := b.state
	b.state = to
	b.generation++
	b.failures = 0
	b.halfOpenAdmitted, b.halfOpenSucceeded = 0, 0
	if to == StateOpen {
		b.openedAt = now
	}
	return Transition{Name: b.name, From: from, To: to, At: now}
}

func (b *Breaker) emit(transitions []Transition) {
	for _, t := range transitions {
		b.observe(t)
	}
}
