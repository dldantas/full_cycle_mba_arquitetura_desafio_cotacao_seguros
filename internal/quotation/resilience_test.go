package quotation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

// memoryCache guarda o que recebe e o TTL pedido. Expira pelo relogio do teste, sem sleep.
type memoryCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]cache.Entry
	expires map[string]time.Time
	ttls    []time.Duration
	down    bool
	writes  int
}

func newMemoryCache(now func() time.Time) *memoryCache {
	return &memoryCache{now: now, entries: map[string]cache.Entry{}, expires: map[string]time.Time{}}
}

func (m *memoryCache) Get(_ context.Context, key string) (cache.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return cache.Entry{}, errors.New("redis: connection refused")
	}
	e, ok := m.entries[key]
	if !ok || !m.now().Before(m.expires[key]) {
		return cache.Entry{}, cache.ErrMiss
	}
	return e, nil
}

func (m *memoryCache) Set(_ context.Context, key string, e cache.Entry, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.down {
		return errors.New("redis: connection refused")
	}
	m.entries[key], m.expires[key] = e, m.now().Add(ttl)
	m.ttls = append(m.ttls, ttl)
	return nil
}

// countingQuoter e seguro para chamadas em paralelo e falha nas parceiras de failing.
type countingQuoter struct {
	mu      sync.Mutex
	calls   map[string]int
	failing map[string]string // parceira -> Kind do erro
}

func newCountingQuoter() *countingQuoter {
	return &countingQuoter{calls: map[string]int{}, failing: map[string]string{}}
}

func (q *countingQuoter) Quote(_ context.Context, p platform.Partner, request any) (partner.Quote, error) {
	q.mu.Lock()
	q.calls[p.Name]++
	kind, fails := q.failing[p.Name]
	q.mu.Unlock()
	if fails {
		return partner.Quote{}, &partner.Error{Partner: p.Name, Kind: kind, Reason: kind}
	}
	broker := request.(partnerRequest).Broker
	return partner.Quote{
		Partner:      p.Name,
		QuoteID:      p.Name + "-" + broker,
		PremiumCents: defaultPremiums()[p.Name] + int64(len(broker)),
		Currency:     "BRL",
	}, nil
}

func (q *countingQuoter) total() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, c := range q.calls {
		n += c
	}
	return n
}

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

const sadTTL = time.Hour // docs/sad.md, secao 4.2

func resilientService(q Quoter, c Cache, clock *testClock) *Service {
	return NewService(threePartners, q,
		WithCache(c, sadTTL), WithPartialResponse(), WithParallelCalls(), WithClock(clock.now))
}

func TestCacheKeyCarriesTheTenantAndHidesTheRisk(t *testing.T) {
	request := validRequest()
	a := CacheKey("corretora-a", "partner-flaky", request)
	b := CacheKey("corretora-b", "partner-flaky", request)

	if a == b {
		t.Fatal("the same risk got the same key for two brokers: broker B would read broker A's premium")
	}
	if !strings.HasPrefix(a, "cotacao:v1:corretora-a:partner-flaky:") {
		t.Fatalf("key %q does not follow cotacao:v1:{tenant_id}:{partner}:{risk}", a)
	}
	for _, personal := range []string{request.Driver.Document, request.Vehicle.Plate} {
		if strings.Contains(a, personal) {
			t.Fatalf("key %q carries personal data (%s) in clear", a, personal)
		}
	}

	untidy := Request{
		Driver:  Driver{Document: " 12345678901 ", BirthYear: 1988},
		Vehicle: Vehicle{Plate: " abc1d23 ", Model: " Gol 1.0 ", Year: 2020, ValueCents: 8500000},
	}
	if err := untidy.Normalize(); err != nil {
		t.Fatal(err)
	}
	if CacheKey("corretora-a", "partner-flaky", untidy) != a {
		t.Fatal("the same risk typed differently produced another key: normalization is not in the key")
	}

	other := validRequest()
	other.Coverage = "third_party"
	if CacheKey("corretora-a", "partner-flaky", other) == a {
		t.Fatal("a different coverage hit the same key")
	}
}

func TestSecondQuoteComesFromCacheWithoutCallingThePartners(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	quoter, store := newCountingQuoter(), newMemoryCache(clock.now)
	service := resilientService(quoter, store, clock)

	first, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("first quote: %v", err)
	}
	if quoter.total() != 3 {
		t.Fatalf("%d partner calls on a cold cache, want 3", quoter.total())
	}
	for _, ttl := range store.ttls {
		if ttl != sadTTL {
			t.Fatalf("entry written with TTL %s, the SAD says %s", ttl, sadTTL)
		}
	}

	clock.t = clock.t.Add(10 * time.Minute)
	second, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("second quote: %v", err)
	}
	if quoter.total() != 3 {
		t.Fatalf("the partners were called again (%d calls) for a cached quote", quoter.total())
	}
	for i, offer := range second.Quotes {
		if offer.Source != SourceCache || offer.AgeSeconds != 600 {
			t.Fatalf("offer %d: source %q age %ds, want cache and 600s", i, offer.Source, offer.AgeSeconds)
		}
		// Rastreavel: o quote_id e o premio sao os da consulta que originou a entrada.
		if offer.QuoteID != first.Quotes[i].QuoteID || offer.PremiumCents != first.Quotes[i].PremiumCents {
			t.Fatalf("offer %d from cache differs from the original purchase: %+v vs %+v", i, offer, first.Quotes[i])
		}
	}

	// Outra corretora, mesmo risco: nao pode ler a entrada da primeira.
	other, err := service.Quote(context.Background(), "corretora-b", validRequest())
	if err != nil {
		t.Fatalf("broker B: %v", err)
	}
	if quoter.total() != 6 {
		t.Fatalf("broker B was served from broker A's cache (%d partner calls, want 6)", quoter.total())
	}
	for _, offer := range other.Quotes {
		if offer.Source != SourcePartner || !strings.HasSuffix(offer.QuoteID, "corretora-b") {
			t.Fatalf("broker B received %+v", offer)
		}
	}

	// Vencido o TTL, a entrada some e a parceira volta a ser consultada.
	clock.t = clock.t.Add(sadTTL)
	if _, err := service.Quote(context.Background(), "corretora-a", validRequest()); err != nil {
		t.Fatal(err)
	}
	if quoter.total() != 9 {
		t.Fatalf("expired entries were served (%d partner calls, want 9)", quoter.total())
	}
}

func TestPartnerDownBecomesAPartialAnswerThatSaysWhoIsMissing(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	quoter := newCountingQuoter()
	quoter.failing["partner-flaky"] = partner.OutcomeShortCircuited
	quoter.failing["partner-degrading"] = partner.OutcomeTimeout

	response, err := resilientService(quoter, newMemoryCache(clock.now), clock).
		Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("one partner answered and the request failed: %v", err)
	}
	if len(response.Quotes) != 1 || response.Quotes[0].Partner != "partner-slow" {
		t.Fatalf("quotes %+v, want only partner-slow", response.Quotes)
	}
	if !response.Degraded {
		t.Fatal("partial answer not flagged as degraded")
	}
	want := map[string]string{"partner-flaky": "short_circuited", "partner-degrading": "timeout"}
	if len(response.Unavailable) != len(want) {
		t.Fatalf("unavailable %+v, want %v", response.Unavailable, want)
	}
	for _, u := range response.Unavailable {
		if want[u.Partner] != u.Reason {
			t.Fatalf("unavailable %+v, want %v", response.Unavailable, want)
		}
	}
}

func TestCachedQuoteCoversAPartnerThatWentDown(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	quoter, store := newCountingQuoter(), newMemoryCache(clock.now)
	service := resilientService(quoter, store, clock)
	if _, err := service.Quote(context.Background(), "corretora-a", validRequest()); err != nil {
		t.Fatal(err)
	}

	quoter.failing["partner-flaky"] = partner.OutcomeShortCircuited // breaker abriu depois
	clock.t = clock.t.Add(5 * time.Minute)
	response, err := service.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if response.Degraded || len(response.Quotes) != 3 {
		t.Fatalf("a valid cached quote should cover the open circuit: %+v", response)
	}
}

func TestNoPartnerAndNoCacheKeepsThe502(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	quoter := newCountingQuoter()
	for _, p := range threePartners {
		quoter.failing[p.Name] = partner.OutcomeError
	}
	store := newMemoryCache(clock.now)
	store.down = true // e o Redis fora: nada para mostrar, e premio inventado e proibido

	handler := NewAPI(resilientService(quoter, store, clock), []string{"corretora-a"}).Routes()
	response := postQuotes(handler, "corretora-a", validBody)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 when nobody has a quote", response.Code)
	}
	var body platform.ErrorBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Message != "partner insurer unavailable" {
		t.Fatalf("body %s, want the documented 502", response.Body)
	}
}

func TestRedisDownFallsBackToThePartners(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	quoter, store := newCountingQuoter(), newMemoryCache(clock.now)
	store.down = true

	response, err := resilientService(quoter, store, clock).Quote(context.Background(), "corretora-a", validRequest())
	if err != nil || len(response.Quotes) != 3 || response.Degraded {
		t.Fatalf("Redis down must cost money, not quotes: %+v, %v", response, err)
	}
	if store.writes != 0 {
		t.Fatalf("%d writes attempted on a Redis that just failed the read: each one pays the Redis timeout again", store.writes)
	}
}

func TestDegradedAnswerIsVisibleInTheHTTPContract(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	quoter := newCountingQuoter()
	quoter.failing["partner-flaky"] = partner.OutcomeError

	handler := NewAPI(resilientService(quoter, newMemoryCache(clock.now), clock), []string{"corretora-a"}).Routes()
	response := postQuotes(handler, "corretora-a", validBody)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 with a partial answer: %s", response.Code, response.Body)
	}

	var body struct {
		Degraded    bool `json:"degraded"`
		Unavailable []struct {
			Partner string `json:"partner"`
			Reason  string `json:"reason"`
		} `json:"unavailable"`
		Quotes []struct {
			Partner      string `json:"partner"`
			PremiumCents int64  `json:"premium_cents"`
			Source       string `json:"source"`
		} `json:"quotes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Degraded || len(body.Unavailable) != 1 || body.Unavailable[0].Partner != "partner-flaky" ||
		body.Unavailable[0].Reason != "error" {
		t.Fatalf("degradation not visible to the broker: %s", response.Body)
	}
	if len(body.Quotes) != 2 || body.Quotes[0].PremiumCents > body.Quotes[1].PremiumCents {
		t.Fatalf("degraded answer must keep the cheapest-first order: %s", response.Body)
	}
	if response.Header().Get(TenantHeader) != "corretora-a" {
		t.Fatal("X-Tenant-Id missing from a degraded answer")
	}
}
