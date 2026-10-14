package platform

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port       string
	Partners   []Partner
	Tenants    []string
	Telemetry  Telemetry
	Resilience Resilience
}

// Resilience reune os numeros que o docs/sad.md (secao 4) defende. Os defaults abaixo SAO os valores
// do SAD; mudar um deles e mudar o SAD junto.
type Resilience struct {
	// PartnerTimeout (PARTNER_TIMEOUT): teto de espera por uma parceira. 2s = maior latencia da
	// partner-slow em repouso (1500 + 200 ms) + 300 ms de folga. O estouro conta como falha no breaker.
	PartnerTimeout time.Duration
	// BreakerFailures (BREAKER_CONSECUTIVE_FAILURES): falhas seguidas que abrem o circuito.
	BreakerFailures int
	// BreakerOpenFor (BREAKER_OPEN_DURATION): tempo aberto antes da primeira sonda.
	BreakerOpenFor time.Duration
	// BreakerHalfOpenCalls (BREAKER_HALF_OPEN_MAX_CALLS): sondas no meio aberto e sucessos para fechar.
	BreakerHalfOpenCalls int
	// CacheTTL (CACHE_TTL): vida de uma cotacao no cache. Nunca acima de 24h, o teto comercial.
	CacheTTL time.Duration
	// RedisAddr (REDIS_ADDR): endereco do Redis do cache.
	RedisAddr string
}

// CommercialCeiling e o prazo em que as seguradoras honram o premio informado (enunciado, "O cenario").
// Cache acima disso nao e cache agressivo, e cotacao que ninguem honra.
const CommercialCeiling = 24 * time.Hour

const (
	defaultPartnerTimeout       = 2 * time.Second
	defaultBreakerFailures      = 6
	defaultBreakerOpenFor       = 10 * time.Second
	defaultBreakerHalfOpenCalls = 2
	defaultCacheTTL             = time.Hour
	defaultRedisAddr            = "localhost:6379"
)

type Partner struct {
	Name    string
	BaseURL string
}

type Telemetry struct {
	ServiceName string
	Endpoint    string
	Enabled     bool
}

const defaultEndpoints = "partner-slow=http://localhost:9001," +
	"partner-flaky=http://localhost:9002," +
	"partner-degrading=http://localhost:9003"

const defaultTenants = "corretora-a,corretora-b"

const defaultCollector = "http://localhost:4317"

const defaultServiceName = "quotation-api"

func LoadConfig(env func(string) string) (Config, error) {
	cfg := Config{Port: text(env, "PORT", "8080")}

	var err error
	if cfg.Partners, err = parsePartners(text(env, "PARTNER_ENDPOINTS", defaultEndpoints)); err != nil {
		return Config{}, err
	}
	if cfg.Tenants, err = parseTenants(text(env, "TENANTS", defaultTenants)); err != nil {
		return Config{}, err
	}
	if cfg.Telemetry, err = parseTelemetry(env); err != nil {
		return Config{}, err
	}
	if cfg.Resilience, err = parseResilience(env); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseResilience(env func(string) string) (Resilience, error) {
	r := Resilience{RedisAddr: strings.TrimSpace(text(env, "REDIS_ADDR", defaultRedisAddr))}

	var err error
	if r.PartnerTimeout, err = duration(env, "PARTNER_TIMEOUT", defaultPartnerTimeout); err != nil {
		return Resilience{}, err
	}
	if r.BreakerFailures, err = count(env, "BREAKER_CONSECUTIVE_FAILURES", defaultBreakerFailures); err != nil {
		return Resilience{}, err
	}
	if r.BreakerOpenFor, err = duration(env, "BREAKER_OPEN_DURATION", defaultBreakerOpenFor); err != nil {
		return Resilience{}, err
	}
	if r.BreakerHalfOpenCalls, err = count(env, "BREAKER_HALF_OPEN_MAX_CALLS", defaultBreakerHalfOpenCalls); err != nil {
		return Resilience{}, err
	}
	if r.CacheTTL, err = duration(env, "CACHE_TTL", defaultCacheTTL); err != nil {
		return Resilience{}, err
	}
	if r.CacheTTL > CommercialCeiling {
		return Resilience{}, fmt.Errorf("CACHE_TTL: %s is above the 24h commercial ceiling", r.CacheTTL)
	}
	if r.RedisAddr == "" {
		return Resilience{}, fmt.Errorf("REDIS_ADDR cannot be empty")
	}
	return r, nil
}

func duration(env func(string) string, key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(env(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: %q is not a positive duration (e.g. 2s, 1h)", key, raw)
	}
	return d, nil
}

func count(env func(string) string, key string, fallback int) (int, error) {
	raw := strings.TrimSpace(env(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s: %q is not a positive integer", key, raw)
	}
	return n, nil
}

func parseTelemetry(env func(string) string) (Telemetry, error) {
	telemetry := Telemetry{
		ServiceName: text(env, "OTEL_SERVICE_NAME", defaultServiceName),
		Endpoint:    strings.TrimSpace(text(env, "OTEL_EXPORTER_OTLP_ENDPOINT", defaultCollector)),
	}

	switch disabled := strings.ToLower(strings.TrimSpace(env("OTEL_SDK_DISABLED"))); disabled {
	case "", "false":
		telemetry.Enabled = true
	case "true":
		return Telemetry{Enabled: false}, nil
	default:
		return Telemetry{}, fmt.Errorf("OTEL_SDK_DISABLED: %q is neither true nor false", disabled)
	}

	if u, err := url.Parse(telemetry.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return Telemetry{}, fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT: %q is not an absolute URL", telemetry.Endpoint)
	}
	return telemetry, nil
}

func parsePartners(raw string) ([]Partner, error) {
	var partners []Partner
	seen := map[string]bool{}

	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		name, endpoint, found := strings.Cut(item, "=")
		name, endpoint = strings.TrimSpace(name), strings.TrimSpace(endpoint)
		if !found || name == "" || endpoint == "" {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: %q is not in the name=url format", item)
		}

		if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: %q is not an absolute URL", endpoint)
		}
		if seen[name] {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: partner %q is repeated", name)
		}

		seen[name] = true
		partners = append(partners, Partner{Name: name, BaseURL: strings.TrimRight(endpoint, "/")})
	}

	if len(partners) == 0 {
		return nil, fmt.Errorf("PARTNER_ENDPOINTS: at least one partner is required")
	}
	return partners, nil
}

func parseTenants(raw string) ([]string, error) {
	var tenants []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tenants = append(tenants, t)
		}
	}
	if len(tenants) == 0 {
		return nil, fmt.Errorf("TENANTS: at least one broker is required")
	}
	return tenants, nil
}

func Environment() func(string) string { return os.Getenv }

func text(env func(string) string, key, fallback string) string {
	if v := env(key); v != "" {
		return v
	}
	return fallback
}
