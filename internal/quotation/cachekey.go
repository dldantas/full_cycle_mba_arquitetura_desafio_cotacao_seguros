package quotation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// CacheKeyVersion muda quando o formato da chave ou do valor muda. Trocar a versao e a invalidacao
// em massa: as chaves antigas deixam de ser lidas e somem sozinhas no TTL (docs/sad.md, secao 6.3).
const CacheKeyVersion = "v1"

// CacheKey monta a chave do cache. Por extenso:
//
//		cotacao:v1:{tenant_id}:{partner}:{sha256(json([document, birth_year, plate, model, year, value_cents, coverage]))}
//
//	  - tenant_id vem do cabecalho X-Tenant-Id ja validado contra TENANTS, nunca do corpo. Sem ele a mesma
//	    placa devolveria a corretora A o premio negociado pela corretora B: erro de preco e incidente de
//	    dados pessoais (LGPD). Fica em claro para permitir o expurgo por corretora (SCAN cotacao:v1:<tenant>:*).
//	  - partner porque o cache e por parceira: sobrevive a uma parceira fora e permite expurgar uma so.
//	  - o risco vai como SHA-256 dos campos ja normalizados por Request.Normalize (placa em maiuscula,
//	    espacos cortados, cobertura default), os mesmos que a parceira recebe. O array JSON evita que
//	    um separador dentro de um campo faca dois riscos diferentes colidirem. CPF e placa nao aparecem
//	    em claro na chave; SHA-256 de CPF e pseudonimizacao, nao anonimizacao, entao o Redis continua
//	    tratado como base com dado pessoal.
func CacheKey(tenant, partner string, r Request) string {
	risk, _ := json.Marshal([]any{
		r.Driver.Document,
		r.Driver.BirthYear,
		r.Vehicle.Plate,
		r.Vehicle.Model,
		r.Vehicle.Year,
		r.Vehicle.ValueCents,
		r.Coverage,
	})
	sum := sha256.Sum256(risk)
	return "cotacao:" + CacheKeyVersion + ":" + tenant + ":" + partner + ":" + hex.EncodeToString(sum[:])
}
