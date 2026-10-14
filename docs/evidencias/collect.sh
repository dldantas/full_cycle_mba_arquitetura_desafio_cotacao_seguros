#!/usr/bin/env bash
# Coleta das evidencias do "depois" (README.md, secao Evidencias). Quatro fases, cada uma partindo de
# ambiente zerado (docker compose down -v). Tudo o que ele escreve fica em docs/evidencias/.
#
#   bash docs/evidencias/collect.sh
#
#   bash docs/evidencias/collect.sh 1 3   # so as fases 1 e 3
#
# Precisa de Docker com Compose v2, curl e Python 3 com matplotlib (para plot_promql.py). Os perfis das
# parceiras no docker-compose.yml nao sao alterados; as fases 2 e 3 sao variacoes DECLARADAS da carga.
set -u
cd "$(dirname "$0")/../.."
E=docs/evidencias
RAW=$E/bruto # saida completa de cada execucao do loadgen
mkdir -p "$RAW"
PROM=http://localhost:9090
JAEGER=http://localhost:16686

counters() { # arquivo, consultas...
  local f=$1; shift
  for qq in "$@"; do
    echo; echo "\$ curl -s --get $PROM/api/v1/query --data-urlencode 'query=$qq'"
    curl -s --get "$PROM/api/v1/query" --data-urlencode "query=$qq" | python -c "
import json,sys
for r in json.load(sys.stdin)['data']['result']:
    m={k:v for k,v in r['metric'].items() if k in ('partner','result','outcome','from','to','server_address','http_response_status_code')}
    print(' ', m, '=', r['value'][1])"
  done >> "$f"
}
report() { sed -n '/^baseline: 10 requests\|^load: [0-9]* requests/,$p' "$1"; }
fresh() {
  docker compose down -v >/dev/null 2>&1; docker compose up -d --build --wait >/dev/null 2>&1
  # O --wait nao espera o Prometheus: sem isto a carga comeca antes da primeira raspagem do Collector e
  # o inicio da execucao some dos graficos.
  until curl -s --get "$PROM/api/v1/query" --data-urlencode 'query=up{job="quotation-api"}' | grep -q '"1"\]'; do sleep 2; done
  until curl -s -o /dev/null -w '%{http_code}' "$JAEGER/" | grep -q 200; do sleep 2; done
  sleep 10 # uma exportacao da API (10s) antes da carga: as series ja existem quando a carga comeca
}
run() { [ ${#PHASES[@]} -eq 0 ] || [[ " ${PHASES[*]} " == *" $1 "* ]]; }
PHASES=("$@") # sem argumentos roda as quatro fases; "bash collect.sh 1 3" roda so a 1 e a 3
trace_with() { # tags-json -> traceID do trace mais antigo com essas tags
  curl -s --get "$JAEGER/api/traces" --data-urlencode service=quotation-api --data-urlencode limit=1000 \
    --data-urlencode lookback=1h --data-urlencode "tags=$1" | python -c "
import json,sys
d=json.load(sys.stdin)['data']
print(sorted(d,key=lambda t:min(s['startTime'] for s in t['spans']))[0]['traceID'] if d else '')"
}
WAIT_EXPORT=15 # metricas exportadas a cada 10s (internal/platform/telemetry.go) + raspagem a cada 5s

if run 1; then
echo "== fase 1: make reproduce padrao"; date -u
fresh
docker compose run --rm --build loadgen > "$RAW/f1-padrao.txt" 2>&1
sleep $WAIT_EXPORT
{ echo '$ make down && make reproduce   # equivalente executado: docker compose down -v && docker compose up -d --build --wait && docker compose run --rm --build loadgen'
  echo "# DEPOIS — codigo final (timeout 2s, breaker 6/10s/2, cache TTL 1h, resposta parcial, paralelo); mesma maquina e mesma carga padrao do antes"
  echo "# executado em $(date -u '+%Y-%m-%d %H:%M UTC')"; echo; report "$RAW/f1-padrao.txt"; } > $E/depois-make-reproduce.txt
f=$E/depois-contadores-execucao-padrao.txt
{ echo "# DEPOIS — contadores de negocio da execucao padrao (depois-make-reproduce.txt), mesma sessao, $(date -u '+%H:%M UTC')"
  echo "# contadores OTel cumulativos desde o boot da API; janela = a execucao inteira"; } > $f
counters $f 'sum by (partner, result) (quotation_cache_requests_total)' \
  'sum(quotation_cache_requests_total{result="hit"}) / sum(quotation_cache_requests_total)' \
  'sum by (partner, outcome) (quotation_partner_duration_seconds_count)' \
  'sum by (server_address, http_response_status_code) (http_client_request_duration_seconds_count)' \
  'sum by (http_response_status_code) (http_server_request_duration_seconds_count{http_route="/quotes"})' \
  'max by (partner) (quotation_partner_circuit_state)'
python $E/plot_promql.py --minutes 2 --title 'DEPOIS - p95 do POST /quotes (http_server_request_duration_seconds, janela [1m])' --unit 's' --ymax 10 \
  --query 'histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket{http_route="/quotes",http_request_method="POST"}[1m])))' --out $E/depois-p95-post-quotes.png
python $E/plot_promql.py --minutes 2 --title 'DEPOIS - p95 por parceira, chamadas que sairam (quotation_partner_duration_seconds, janela [1m])' --unit 's' --ymax 2.5 \
  --query 'histogram_quantile(0.95, sum by (partner, le) (rate(quotation_partner_duration_seconds_bucket{outcome!="short_circuited"}[1m])))' --out $E/depois-p95-por-parceira.png
id=$(trace_with '{"quotation.cache_hits":"3"}'); curl -s "$JAEGER/api/traces/$id" -o $E/depois-trace-servido-de-cache.json; echo "cache trace $id"
id=$(trace_with '{"quotation.degraded":"true"}'); curl -s "$JAEGER/api/traces/$id" -o $E/depois-trace-resposta-parcial.json; echo "degraded trace $id"

fi

if run 2; then
echo "== fase 2: breaker, carga sequencial"; date -u
fresh
docker compose run --rm loadgen -concurrency 1 -baseline 0 -requests 140 -distinct 100000 -duration 9m > "$RAW/f2-breaker.txt" 2>&1
sleep $WAIT_EXPORT
{ echo '$ make down && docker compose up -d --build --wait && make load ARGS="-concurrency 1 -baseline 0 -requests 140 -distinct 100000 -duration 9m"'
  echo '# equivalente executado: docker compose down -v && docker compose up -d --build --wait && docker compose run --rm loadgen -concurrency 1 -baseline 0 -requests 140 -distinct 100000 -duration 9m'
  echo '# VARIACAO DECLARADA DA CARGA, nao dos perfis das parceiras (que seguem os defaults do compose): uma requisicao por vez, cada uma com'
  echo '# um risco novo, para todo pedido ser miss e a partner-flaky receber a sequencia deterministica na ordem. Objetivo: ver o breaker'
  echo '# abrir na sequencia 54, como preve internal/partner/breaker_test.go. Na carga padrao o cache absorve a partner-flaky e o breaker nao abre.'
  echo "# executado em $(date -u '+%Y-%m-%d %H:%M UTC')"; echo; report "$RAW/f2-breaker.txt"; } > $E/breaker-make-load-sequencial.txt
{ echo '$ docker compose logs -t quotation-api | grep "circuit breaker"'; echo '# mesma sessao de breaker-make-load-sequencial.txt; horarios em UTC'
  docker compose logs -t quotation-api 2>/dev/null | grep "circuit breaker" | sed 's/^cotacao-quotation-api  | //'; } > $E/breaker-transicoes-log.txt
f=$E/breaker-contadores.txt
echo "# contadores da sessao de breaker-make-load-sequencial.txt, $(date -u '+%H:%M UTC') (cumulativos desde o boot)" > $f
counters $f 'sum by (partner, outcome) (quotation_partner_duration_seconds_count)' \
  'sum by (partner, from, to) (quotation_partner_circuit_transitions_total)' \
  'sum by (server_address, http_response_status_code) (http_client_request_duration_seconds_count)' \
  'sum by (partner) (quotation_partner_duration_seconds_count{outcome!="short_circuited"}) - sum by (partner) (quotation_partner_duration_seconds_bucket{le="2.0",outcome!="short_circuited"})'
echo '# a ultima consulta e (chamadas que sairam) - (chamadas com duracao <= 2s): 0 = nenhuma passou do PARTNER_TIMEOUT' >> $f
# Em que chamada a partner-flaky abriu: conta os spans HTTP de saida para ela antes do primeiro curto-circuito.
{ echo; echo "\$ # Jaeger API: traces POST /quotes da ultima hora em ordem de inicio; conta spans 'HTTP POST' com server.address=partner-flaky ate o primeiro trace com quotation.circuit.short_circuited=true na partner-flaky"
  curl -s --get "$JAEGER/api/traces" --data-urlencode service=quotation-api --data-urlencode 'operation=POST /quotes' --data-urlencode limit=1000 \
    --data-urlencode lookback=1h | python -c "
import json,sys
traces=sorted(json.load(sys.stdin)['data'], key=lambda t:min(s['startTime'] for s in t['spans']))
tags=lambda s:{g['key']:g['value'] for g in s['tags']}
calls=0
for t in traces:
    if any(tags(s).get('quotation.circuit.short_circuited') and tags(s).get('quotation.partner')=='partner-flaky' for s in t['spans']):
        print(f'  {len(traces)} traces POST /quotes; chamadas HTTP a partner-flaky antes do primeiro curto-circuito: {calls}'); break
    calls+=sum(1 for s in t['spans'] if s['operationName']=='HTTP POST' and tags(s).get('server.address')=='partner-flaky')
"; } >> $f
python $E/plot_promql.py --minutes 6 --step 2s --step-plot --yticks '0=fechado (0),1=meio aberto (1),2=aberto (2)' --ymax 2.3 \
  --title 'Estado do breaker por parceira - carga sequencial (gauge coletado a cada 10s)' --query 'max by (partner) (quotation_partner_circuit_state)' --out $E/breaker-estado-no-tempo.png
python $E/plot_promql.py --minutes 6 --step 2s --step-plot --unit 'transicoes (acumulado)' \
  --title 'Transicoes do breaker da partner-flaky por origem e destino (quotation_partner_circuit_transitions_total)' \
  --query 'sum by (from, to) (quotation_partner_circuit_transitions_total{partner="partner-flaky"})' --out $E/breaker-transicoes-no-tempo.png
id=$(trace_with '{"quotation.circuit.short_circuited":"true"}'); curl -s "$JAEGER/api/traces/$id" -o $E/breaker-trace-curto-circuitado.json; echo "short-circuit trace $id"

fi

if run 3; then
echo "== fase 3: aquecimento do cache"; date -u
fresh
docker compose run --rm loadgen -concurrency 1 -baseline 0 -requests 600 -distinct 40 -duration 5m > "$RAW/f3-aquecimento.txt" 2>&1
sleep $WAIT_EXPORT
{ echo '$ make down && docker compose up -d --build --wait && make load ARGS="-concurrency 1 -baseline 0 -requests 600 -distinct 40 -duration 5m"'
  echo '# equivalente executado: docker compose down -v && docker compose up -d --build --wait && docker compose run --rm loadgen -concurrency 1 -baseline 0 -requests 600 -distinct 40 -duration 5m'
  echo '# VARIACAO DECLARADA DA CARGA: na carga padrao o baseline dura 8s e a carga 0,3s, menos que um intervalo de exportacao (10s), e o grafico'
  echo '# vira um plato. Aqui 40 riscos distintos, 1 em voo: ~70s de cache frio (hit rate 0) e depois o aquecimento. O valor final e propriedade da carga;'
  echo '# o hit rate da secao 8 do SAD sai do pressuposto P-1 (recotacao), nao daqui.'
  echo "# executado em $(date -u '+%Y-%m-%d %H:%M UTC')"; echo; report "$RAW/f3-aquecimento.txt"; } > $E/cache-aquecimento-make-load.txt
# "or 0 * total": sem isto a razao nao existe enquanto nao houver nenhum hit, e a fase fria some do grafico.
python $E/plot_promql.py --minutes 3 --step 2s --ymax 1.05 --unit 'hit rate acumulado (0-1)' \
  --title 'Hit rate do cache subindo com o aquecimento, por parceira (quotation_cache_requests_total, acumulado desde o boot)' \
  --query '(sum by (partner) (quotation_cache_requests_total{result="hit"}) or 0 * sum by (partner) (quotation_cache_requests_total)) / sum by (partner) (quotation_cache_requests_total)' --out $E/cache-hit-rate-aquecimento.png
fi

if run 4; then
echo "== fase 4: Redis fora"; date -u
fresh
docker compose stop redis > "$RAW/f4-redis-fora.txt" 2>&1
docker compose run --rm --no-deps loadgen >> "$RAW/f4-redis-fora.txt" 2>&1 # sem --no-deps o compose religa o redis pelo depends_on
sleep $WAIT_EXPORT
f=$E/redis-fora-make-load.txt
{ echo '# ENSAIO do cenario (a) do SAD, secao 7.2 — Redis parado, carga padrao, codigo final (sem gravacao apos leitura com erro).'
  echo '$ make down && docker compose up -d --build --wait && docker compose stop redis && make load'
  echo '# equivalente executado: ... && docker compose run --rm --no-deps loadgen   (sem --no-deps o compose religa o redis pelo depends_on)'
  echo "# executado em $(date -u '+%Y-%m-%d %H:%M UTC')"; echo; grep -i "redis" "$RAW/f4-redis-fora.txt"; report "$RAW/f4-redis-fora.txt"
  echo; echo '$ docker compose logs -t quotation-api | grep "circuit breaker"'
  docker compose logs -t quotation-api 2>/dev/null | grep "circuit breaker" | sed 's/^cotacao-quotation-api  | //'; } > $f
counters $f 'sum by (partner, result) (quotation_cache_requests_total)' 'sum by (partner, outcome) (quotation_partner_duration_seconds_count)'
python $E/plot_promql.py --minutes 2 --step 2s --step-plot --yticks '0=fechado (0),1=meio aberto (1),2=aberto (2)' --ymax 2.3 \
  --title 'Redis fora: estado do breaker por parceira (a partner-degrading afunda, estoura 2s e abre)' --query 'max by (partner) (quotation_partner_circuit_state)' --out $E/redis-fora-estado-breaker.png
fi

echo "== fim"; date -u
