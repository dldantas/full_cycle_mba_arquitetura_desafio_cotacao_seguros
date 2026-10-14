# Prumo Cota: proteção da fronteira com as seguradoras

Resolução do desafio *Fundamentos de Arquitetura de Solução* (o enunciado original está em
[docs/enunciado.md](docs/enunciado.md)).

- **SAD** (entrega 1): [docs/sad.md](docs/sad.md)
- **Evidências** (entrega 2): [docs/evidencias/](docs/evidencias/), indexadas na tabela abaixo

## O coração da solução

Protegi a fronteira entre a `quotation-api` e as três seguradoras, que é onde a plataforma perdia
dinheiro e disponibilidade. Cada chamada a uma seguradora agora tem teto de 2 s, e o estouro conta
como falha num circuit breaker **por seguradora** (6 falhas seguidas abrem, 10 s aberto, 2 sondas no
meio aberto). Aberto quer dizer que a chamada não sai: a consulta de R$ 0,04 não é comprada e a
corretora não espera. Antes de chamar, a API procura a proposta daquela seguradora num cache Redis de
1 h cuja chave começa pela corretora (`cotacao:v1:{tenant_id}:...`), porque a mesma placa tem preço
diferente para cada corretora e servir a de uma à outra é incidente de LGPD. Quando uma seguradora não
responde, a cotação sai com as outras e diz quem faltou (`degraded`, `unavailable`), em vez de virar
502. As três são consultadas em paralelo. Com a mesma carga do `make reproduce`, o p95 sob carga caiu de
8,01 s para 202 ms, o sucesso subiu de 60% para 100% e as consultas compradas
foram de 545 para 57. O custo disso: um Redis que agora é dependência (opcional:
quando ele cai, a cotação continua, só que mais cara e ~0,2 s mais lenta), um breaker escrito por mim
que precisa de manutenção, e uma corretora que às vezes recebe duas propostas em vez de três. Pelo SAD
(seção 8), o cache se paga em menos de um mês se 40% das cotações forem recotações dentro de 1 h, e
deixa de se pagar abaixo de 2,5%.

## Como subir e reproduzir

Pré-requisitos: Docker com Compose v2. `make` e Go 1.25 são opcionais: cada alvo do `Makefile` é uma
linha de `docker compose` ou de `go` (`make help`).

```bash
make down && make reproduce   # sobe tudo, espera ficar saudável e roda a carga padrão (o "depois")
make test                     # a suíte inteira (testes que já vinham + os desta entrega)
make smoke                    # o smoke test de factibilidade do breaker
```

Sem `make` nem Go no host (foi o meu caso, Windows), os equivalentes são:

```bash
docker compose down -v && docker compose up -d --build --wait && docker compose run --rm --build loadgen
docker run --rm -v "$PWD":/src -w /src golang:1.25 make test
```

Para ver o breaker abrir (a carga padrão quase não chama a `partner-flaky`, porque o cache absorve):

```bash
make down && docker compose up -d --build --wait
make load ARGS="-concurrency 1 -baseline 0 -requests 140 -distinct 100000 -duration 9m"
docker compose logs quotation-api | grep "circuit breaker"
```

Os parâmetros ficam no `docker-compose.yml` (serviço `quotation-api`) e têm os mesmos defaults em
`internal/platform/config.go`: `PARTNER_TIMEOUT=2s`, `BREAKER_CONSECUTIVE_FAILURES=6`,
`BREAKER_OPEN_DURATION=10s`, `BREAKER_HALF_OPEN_MAX_CALLS=2`, `CACHE_TTL=1h` (o código recusa mais de
24 h) e `REDIS_ADDR=redis:6379`. Os perfis das parceiras não foram alterados.

## O que foi implementado e o que ficou como proposta

| Implementado (código + teste) | Onde |
|---|---|
| Timeout de 2 s na chamada à parceira; o estouro conta como falha no breaker | `internal/partner/client.go` |
| Circuit breaker com três estados nomeados (`StateClosed`, `StateHalfOpen`, `StateOpen`), um por parceira; aberto não chama a parceira | `internal/resilience/breaker.go`, `internal/partner/client.go` |
| Cache por parceira no Redis, TTL de 1 h, chave com a corretora | `internal/cache/redis.go`, `internal/quotation/cachekey.go`, `internal/quotation/service.go` |
| Fallback de resposta parcial, visível no contrato | `internal/quotation/service.go`, `internal/quotation/request.go` |
| Paralelização da agregação (bônus) | `internal/quotation/service.go` |
| Métricas e marcações de trace de negócio | `internal/partner/telemetry.go`, `internal/quotation/telemetry.go` |
| Parâmetros configuráveis, com o teto de 24 h validado | `internal/platform/config.go`, `docker-compose.yml` |

| Proposta (só no SAD) | Onde |
|---|---|
| Registro de auditoria por cotação apresentada, em armazenamento WORM por 5 anos | SAD 4.5, 5.2 e 8.2 |
| Regras de alerta A-1 a A-4 (as consultas PromQL rodam hoje; não há Alertmanager no compose) | SAD 6.2 |
| Hospedagem em nuvem pública no Brasil e DR de região | SAD 5.2 e 7.4 |
| Bulkhead e coalescência de misses, breaker compartilhado entre réplicas, invalidação por evento, cota por corretora | SAD 4.6 (limites L-1 a L-4) |

**A chave do cache, por extenso** (montada em `internal/quotation/cachekey.go`):

```
cotacao:v1:{tenant_id}:{partner}:{sha256(json([document, birth_year, plate, model, year, value_cents, coverage]))}
```

`tenant_id` vem do cabeçalho `X-Tenant-Id` já validado contra `TENANTS`, e não do corpo. O risco entra
como hash dos campos já normalizados, para que CPF e placa não fiquem em claro na chave. A
invalidação acontece pelo TTL de 1 h; a troca de `v1` invalida tudo; o expurgo manual por corretora ou
por parceira é feito por padrão de chave (SAD 6.3). Falhas não são cacheadas, e entrada vencida não é
servida nem com a parceira fora.

**Mudança no contrato de resposta** (`internal/quotation/request.go`; os campos são aditivos e nenhum
foi removido):

| Campo | Significado |
|---|---|
| `degraded` | `true` quando faltou alguma parceira na resposta |
| `unavailable[]` | `{partner, reason}` de cada parceira que faltou; `reason` é `error`, `timeout` ou `short_circuited` |
| `quotes[].source` | `partner` (comprada agora) ou `cache` |
| `quotes[].quoted_at`, `quotes[].age_seconds` | quando a proposta foi comprada e há quanto tempo; o `quote_id` é sempre o da consulta original |

Quando nenhuma parceira e nenhum cache têm proposta, a resposta continua sendo o 502 de antes,
`{"error":"partner insurer unavailable","partner":"..."}`.

## Métricas e atributos criados

| Nome no Prometheus | Tipo | Atributos |
|---|---|---|
| `quotation_partner_circuit_state` | gauge (0 fechado, 1 meio aberto, 2 aberto) | `partner` |
| `quotation_partner_circuit_transitions_total` | contador | `partner`, `from`, `to` |
| `quotation_cache_requests_total` | contador | `result` (`hit`, `miss`, `error`), `partner`, `tenant_id` |
| `quotation_partner_duration_seconds` | histograma (s) | `partner`, `outcome` (`success`, `error`, `timeout`, `short_circuited`, `canceled`, `rejected`) |

No OTel, os nomes de origem são `quotation.partner.circuit.state`, `quotation.partner.circuit.transitions`,
`quotation.cache.requests` e `quotation.partner.duration`. Para a latência da borda HTTP continuo usando
`http_client_request_duration_seconds` e `http_server_request_duration_seconds`, que já vinham no
starter. A métrica nova, `quotation_partner_duration_seconds`, conta exatamente as mesmas chamadas, só
que com o resultado de domínio: em [depois-contadores-execucao-padrao.txt](docs/evidencias/depois-contadores-execucao-padrao.txt)
as duas contagens coincidem.

Atributos de span: um span `quotation.partner` por parceira com `quotation.partner`,
`quotation.cache.hit`, `quotation.source`, `quotation.cache.age_seconds`, `quotation.circuit.state`,
`quotation.circuit.short_circuited` (mais o evento `circuit breaker open: partner not called`),
`quotation.partner.outcome` e `quotation.unavailable_reason`. No span raiz: `quotation.tenant_id`,
`quotation.degraded`, `quotation.cache_hits` e `quotation.quotes`. CPF, placa e `quote_id` não aparecem
em nenhum atributo de span ou de métrica.

## Evidências

Todas medidas por mim, nesta máquina (Windows 11, Docker Desktop 27.4, 16 CPUs). O "antes" foi
coletado à mão no commit `cbc3c1e`, antes de qualquer alteração. Todo o "depois" é gerado por
[collect.sh](docs/evidencias/collect.sh) (`bash docs/evidencias/collect.sh`, ~12 min, quatro fases,
cada uma com o ambiente zerado), e a saída completa de cada execução do `loadgen` fica em
[docs/evidencias/bruto/](docs/evidencias/bruto/). As consultas PromQL
estão impressas no rodapé de cada PNG e no `.json` de mesmo nome, que guarda a resposta crua do
Prometheus com início, fim e passo. Os gráficos foram gerados com
[plot_promql.py](docs/evidencias/plot_promql.py) durante a execução, porque o Prometheus não tem
volume e retém só 1 h.

| Evidência | Arquivo | Comando / consulta | O que se vê |
|---|---|---|---|
| Relatório antes | [antes-make-reproduce.txt](docs/evidencias/antes-make-reproduce.txt) | `make down && make reproduce` no commit `cbc3c1e` | 60% de sucesso e p95 de 8,01 s sob carga; 5 falhas no baseline |
| Relatório depois | [depois-make-reproduce.txt](docs/evidencias/depois-make-reproduce.txt) | o mesmo comando, código final | 200/200 com sucesso e p95 de 202 ms sob carga; baseline 10/10 com p95 de 1,68 s |
| Consultas compradas antes/depois | [antes-consultas-por-cotacao.txt](docs/evidencias/antes-consultas-por-cotacao.txt), [depois-contadores-execucao-padrao.txt](docs/evidencias/depois-contadores-execucao-padrao.txt) | consultas no arquivo | 545 consultas para 125 cotações entregues antes; 57 consultas para 210 depois |
| Saída do `make test` | [make-test.txt](docs/evidencias/make-test.txt) | `make test`, `make smoke` | a suíte inteira passando, os testes que já vinham e os novos |
| Trace com breaker aberto | [breaker-trace-curto-circuitado.json](docs/evidencias/breaker-trace-curto-circuitado.json) | Jaeger, Last Hour, Tags `quotation.circuit.short_circuited=true`, exportado por `GET /api/traces/{id}` | o span da `partner-flaky` dura microssegundos, tem o evento `circuit breaker open: partner not called` e não tem `HTTP POST` filho |
| Trace servido de cache | [depois-trace-servido-de-cache.json](docs/evidencias/depois-trace-servido-de-cache.json) | Jaeger, Last Hour, Tags `quotation.cache_hits=3` | a cotação inteira em microssegundos, com 3 spans `quotation.cache.hit=true` e nenhum span de saída para as parceiras |
| Estado do breaker no tempo | [breaker-estado-no-tempo.png](docs/evidencias/breaker-estado-no-tempo.png), [breaker-transicoes-no-tempo.png](docs/evidencias/breaker-transicoes-no-tempo.png), [breaker-transicoes-log.txt](docs/evidencias/breaker-transicoes-log.txt) | `max by (partner) (quotation_partner_circuit_state)` e `sum by (from, to) (quotation_partner_circuit_transitions_total{partner="partner-flaky"})`, janela de 6 min, passo 2 s; carga em [breaker-make-load-sequencial.txt](docs/evidencias/breaker-make-load-sequencial.txt) | o degrau fechado → aberto → meio aberto → fechado da `partner-flaky` |
| Hit rate do cache | [cache-hit-rate-aquecimento.png](docs/evidencias/cache-hit-rate-aquecimento.png); carga em [cache-aquecimento-make-load.txt](docs/evidencias/cache-aquecimento-make-load.txt) | `(sum by (partner) (quotation_cache_requests_total{result="hit"}) or 0 * sum by (partner) (quotation_cache_requests_total)) / sum by (partner) (quotation_cache_requests_total)`, janela de 3 min (o `or 0 * ...` faz a razão valer 0 antes do primeiro hit) | 0 durante os ~70 s de cache frio, depois sobe para 93% (`partner-slow`, `partner-degrading`) e 69% (`partner-flaky`, que só grava quando responde) |
| p95 do `POST /quotes`, antes | [antes-p95-post-quotes.png](docs/evidencias/antes-p95-post-quotes.png) | `histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket{http_route="/quotes",http_request_method="POST"}[1m])))`, janela de 8 min | ~9,7 s sob carga (estimativa do histograma, no bucket de 7,5 a 10 s; o relatório de texto dá 8,01 s) |
| p95 do `POST /quotes`, depois | [depois-p95-post-quotes.png](docs/evidencias/depois-p95-post-quotes.png) | a mesma consulta e a mesma escala de 0 a 10 s, janela de 2 min | ~0,2 s na mesma escala; poucos pontos, porque a carga padrão inteira dura 0,3 s (ver Limitações) |

Evidências complementares:
- [antes-trace-serie-lento.json](docs/evidencias/antes-trace-serie-lento.json) e [antes-trace-erro-flaky.json](docs/evidencias/antes-trace-erro-flaky.json): a cascata em série de 8 s e o 502 em que o span da `partner-degrading` nem existe;
- [antes-p95-por-parceira.png](docs/evidencias/antes-p95-por-parceira.png) e [depois-p95-por-parceira.png](docs/evidencias/depois-p95-por-parceira.png): a latência por parceira;
- [depois-trace-resposta-parcial.json](docs/evidencias/depois-trace-resposta-parcial.json): uma resposta 200 com `quotation.degraded=true`, em que a `partner-flaky` respondeu 503;
- [breaker-contadores.txt](docs/evidencias/breaker-contadores.txt): em que chamada o breaker abriu, quantas foram curto-circuitadas e nenhuma chamada acima de 2 s;
- [redis-fora-make-load.txt](docs/evidencias/redis-fora-make-load.txt) e [redis-fora-estado-breaker.png](docs/evidencias/redis-fora-estado-breaker.png): o ensaio do cenário (a) do SAD, com o Redis parado; [redis-fora-1-antes-da-correcao.txt](docs/evidencias/redis-fora-1-antes-da-correcao.txt) é a primeira execução, que mostrou um custo de ~0,4 s por cotação e levou à correção descrita no SAD (7.2).

## Limitações do processo, documentadas

- `make` e Go não estão instalados na minha máquina. Rodei os mesmos comandos dos alvos do `Makefile`
  pelo `docker compose` e, para `make test`, rodei o `make` dentro da imagem `golang:1.25`. Os
  comandos exatos estão no cabeçalho de cada evidência.
- Na carga padrão, a fase de carga dura ~0,3 s e o baseline ~8 s, menos que o intervalo de exportação
  de métricas do SDK (10 s, em `internal/platform/telemetry.go`, que não alterei). Por isso o gráfico de
  p95 do "depois" tem poucos pontos, e as evidências de hit rate e de estado do breaker vêm de
  **variações declaradas da carga**, com os perfis das parceiras intocados. O relatório de texto
  antes/depois é sempre da carga padrão.
- O `X-Partner-Seq` não é exposto pela API. A afirmação "abriu na 54ª chamada" vem da contagem de spans
  `HTTP POST` no Jaeger ([breaker-contadores.txt](docs/evidencias/breaker-contadores.txt)) e do teste
  determinístico `TestBreakerOpensInsideTheBurstAndStopsCallingThePartner`.

## O que eu faria diferente com mais tempo

- **Coalescência de misses (single-flight por chave) e bulkhead por parceira.** Na carga padrão, 5
  riscos distintos geraram 29 compras na `partner-flaky`, porque misses concorrentes do mesmo
  risco compram em dobro; e o ensaio com o Redis fora mostrou a `partner-degrading` afundando em 2 s
  (limite L-1 do SAD).
- **Breaker também para o Redis.** Hoje, com o Redis fora, cada cotação paga um `DialTimeout` de 200 ms
  antes de desistir dele.
- **Medir a premissa P-1 de verdade.** A conta da seção 8 depende dela. Eu instrumentaria a idade da
  entrada no momento do hit (histograma de `age_seconds`) para validar a curva de recotação nas duas
  primeiras semanas.
- **Timeout por parceira** em vez de um só, para que a `partner-degrading` vire sinal com 7 chamadas
  simultâneas, e não com 12.
- Escrever de fato o registro de auditoria (4.5) e as regras de alerta (6.2) como código.
