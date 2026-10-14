package quotation

import (
	"fmt"
	"strings"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
)

type Request struct {
	Driver   Driver  `json:"driver"`
	Vehicle  Vehicle `json:"vehicle"`
	Coverage string  `json:"coverage"`
}

type Driver struct {
	Document  string `json:"document"`
	BirthYear int    `json:"birth_year"`
}

type Vehicle struct {
	Plate      string `json:"plate"`
	Model      string `json:"model"`
	Year       int    `json:"year"`
	ValueCents int64  `json:"value_cents"`
}

var coverages = map[string]bool{"comprehensive": true, "third_party": true}

const defaultCoverage = "comprehensive"

func (r *Request) Normalize() error {
	r.Coverage = strings.TrimSpace(r.Coverage)
	if r.Coverage == "" {
		r.Coverage = defaultCoverage
	}

	r.Driver.Document = strings.TrimSpace(r.Driver.Document)
	r.Vehicle.Plate = strings.ToUpper(strings.TrimSpace(r.Vehicle.Plate))
	r.Vehicle.Model = strings.TrimSpace(r.Vehicle.Model)

	switch {
	case r.Driver.Document == "":
		return fmt.Errorf("driver.document is required")
	case r.Driver.BirthYear < 1900:
		return fmt.Errorf("driver.birth_year is required and must be a valid year")
	case r.Vehicle.Plate == "":
		return fmt.Errorf("vehicle.plate is required")
	case r.Vehicle.Year < 1900:
		return fmt.Errorf("vehicle.year is required and must be a valid year")
	case r.Vehicle.ValueCents <= 0:
		return fmt.Errorf("vehicle.value_cents must be greater than zero")
	case !coverages[r.Coverage]:
		return fmt.Errorf("coverage must be comprehensive or third_party")
	}
	return nil
}

type partnerRequest struct {
	Broker string `json:"broker"`
	Request
}

// Response e o contrato do POST /quotes. Os campos degraded e unavailable sao o fallback de resposta
// parcial (docs/sad.md, secao 4.3): a corretora sabe, lendo o corpo, que faltou parceira e por que.
// Cada cotacao diz de onde veio (source) e com que idade (age_seconds), e o quote_id e sempre o que a
// parceira emitiu: premio e sempre de parceira ou de cache rastreavel, nunca calculado aqui.
type Response struct {
	TenantID    string        `json:"tenant_id"`
	Quotes      []Offer       `json:"quotes"`
	Degraded    bool          `json:"degraded"`
	Unavailable []Unavailable `json:"unavailable,omitempty"`
	ElapsedMs   int64         `json:"elapsed_ms"`
}

const (
	SourcePartner = "partner"
	SourceCache   = "cache"
)

type Offer struct {
	partner.Quote
	Source     string    `json:"source"`
	QuotedAt   time.Time `json:"quoted_at"`
	AgeSeconds int64     `json:"age_seconds"`
}

// Unavailable diz qual parceira ficou de fora e por que: short_circuited (breaker aberto, a parceira
// nem foi chamada), timeout (passou de PARTNER_TIMEOUT), error (5xx ou falha de transporte).
type Unavailable struct {
	Partner string `json:"partner"`
	Reason  string `json:"reason"`
}
