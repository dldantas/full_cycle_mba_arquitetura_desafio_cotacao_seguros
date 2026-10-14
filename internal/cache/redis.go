// Package cache guarda, no Redis, a cotacao de cada parceira (docs/sad.md, secao 4.2).
//
// O pacote nao monta a chave: quem monta e internal/quotation/cachekey.go, que e onde a corretora e o
// risco normalizado se encontram. Aqui so se le e se escreve, com TTL obrigatorio em toda escrita.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Entry e o que fica no Redis: a cotacao da parceira como ela veio (inclusive o quote_id, que liga a
// resposta servida de cache a consulta que a originou) e o instante em que foi comprada. Nao ha CPF
// nem placa no valor.
type Entry struct {
	Partner         string    `json:"partner"`
	QuoteID         string    `json:"quote_id"`
	PremiumCents    int64     `json:"premium_cents"`
	Currency        string    `json:"currency"`
	CoverageCents   int64     `json:"coverage_cents"`
	ValidForSeconds int64     `json:"valid_for_seconds"`
	QuotedAt        time.Time `json:"quoted_at"`
}

var ErrMiss = errors.New("cache miss")

type Redis struct {
	client *redis.Client
}

// Timeouts curtos de proposito: o cache e um atalho, nao uma dependencia. Se o Redis demorar mais
// que isso, a leitura vira miss e a cotacao segue para a parceira (fail-open, SAD secao 7, cenario a).
const (
	dialTimeout  = 200 * time.Millisecond
	ioTimeout    = 100 * time.Millisecond
	poolTimeout  = 150 * time.Millisecond
	maxRetries   = -1  // sem retry: retry esta fora do escopo do desafio e dobraria a espera
	poolSize     = 160 // 50 cotacoes em voo x 3 parceiras = 150 leituras simultaneas, sem fila no pool
	minIdleConns = 5
)

func NewRedis(addr string) *Redis {
	return &Redis{client: redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  dialTimeout,
		ReadTimeout:  ioTimeout,
		WriteTimeout: ioTimeout,
		PoolTimeout:  poolTimeout,
		MaxRetries:   maxRetries,
		PoolSize:     poolSize,
		MinIdleConns: minIdleConns,
	})}
}

func (r *Redis) Get(ctx context.Context, key string) (Entry, error) {
	raw, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return Entry{}, ErrMiss
	}
	if err != nil {
		return Entry{}, err
	}
	var entry Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// Set grava com TTL. Nao existe escrita sem prazo: e o TTL que cumpre o prazo de descarte da LGPD
// num Redis que nao persiste nada.
func (r *Redis) Set(ctx context.Context, key string, entry Entry, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("cache: refusing to write without a TTL")
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, key, raw, ttl).Err()
}

func (r *Redis) Close() error { return r.client.Close() }
