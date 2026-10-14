package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestEntryLivesExactlyTheTTL(t *testing.T) {
	server := miniredis.RunT(t)
	r := NewRedis(server.Addr())
	t.Cleanup(func() { _ = r.Close() })

	key := "cotacao:v1:corretora-a:partner-slow:abc"
	quotedAt := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	want := Entry{Partner: "partner-slow", QuoteID: "partner-slow-1", PremiumCents: 123456, Currency: "BRL", QuotedAt: quotedAt}
	if err := r.Set(context.Background(), key, want, time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if ttl := server.TTL(key); ttl != time.Hour {
		t.Fatalf("TTL in Redis %s, want 1h", ttl)
	}
	got, err := r.Get(context.Background(), key)
	if err != nil || got != want {
		t.Fatalf("Get = %+v, %v; want %+v", got, err, want)
	}

	server.FastForward(time.Hour) // o relogio do miniredis anda; nenhum sleep
	if _, err := r.Get(context.Background(), key); !errors.Is(err, ErrMiss) {
		t.Fatalf("entry survived its TTL: %v", err)
	}
}

func TestNoWriteWithoutTTL(t *testing.T) {
	server := miniredis.RunT(t)
	r := NewRedis(server.Addr())
	t.Cleanup(func() { _ = r.Close() })

	if err := r.Set(context.Background(), "k", Entry{}, 0); err == nil {
		t.Fatal("an entry without TTL was written: personal data with no discard deadline")
	}
	if server.Exists("k") {
		t.Fatal("key exists in Redis")
	}
}

func TestRedisDownIsAnErrorNotAMiss(t *testing.T) {
	server := miniredis.RunT(t)
	r := NewRedis(server.Addr())
	t.Cleanup(func() { _ = r.Close() })
	server.Close()

	_, err := r.Get(context.Background(), "k")
	if err == nil || errors.Is(err, ErrMiss) {
		t.Fatalf("Redis down returned %v; the service must be able to count it as error", err)
	}
}
