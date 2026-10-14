"""Renderiza uma consulta PromQL (query_range) em PNG e guarda a resposta crua em JSON.

Uso (com o ambiente de pe, durante a execucao — o Prometheus do compose nao tem volume):

    python docs/evidencias/plot_promql.py --query '<PromQL>' --minutes 10 \
        --title 'p95 POST /quotes' --unit s --out docs/evidencias/antes-p95.png

O grafico e a afirmacao; a consulta impressa no proprio PNG e no JSON e a fonte.
"""

import argparse
import json
import time
import urllib.parse
import urllib.request
from datetime import datetime

import matplotlib

matplotlib.use("Agg")
import matplotlib.dates as mdates  # noqa: E402
import matplotlib.pyplot as plt  # noqa: E402


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--prometheus", default="http://localhost:9090")
    parser.add_argument("--query", required=True)
    parser.add_argument("--minutes", type=float, default=10)
    parser.add_argument("--step", default="5s")
    parser.add_argument("--title", required=True)
    parser.add_argument("--unit", default="")
    parser.add_argument("--ymax", type=float)
    parser.add_argument("--step-plot", action="store_true", help="desenha degraus (estado do breaker)")
    parser.add_argument("--yticks", help="rotulos do eixo y, ex.: 0=fechado,1=meio-aberto,2=aberto")
    parser.add_argument("--out", required=True)
    args = parser.parse_args()

    end = time.time()
    start = end - args.minutes * 60
    params = urllib.parse.urlencode(
        {"query": args.query, "start": f"{start:.0f}", "end": f"{end:.0f}", "step": args.step}
    )
    with urllib.request.urlopen(f"{args.prometheus}/api/v1/query_range?{params}") as response:
        payload = json.load(response)

    with open(args.out.rsplit(".", 1)[0] + ".json", "w", encoding="utf-8") as raw:
        json.dump({"query": args.query, "start": start, "end": end, "step": args.step, "response": payload},
                  raw, indent=1)

    fig, ax = plt.subplots(figsize=(11, 5.2), dpi=110)
    series = payload["data"]["result"]
    for s in series:
        xs = [datetime.fromtimestamp(float(t)) for t, _ in s["values"]]
        ys = [float(v) if v not in ("NaN", "+Inf") else float("nan") for _, v in s["values"]]
        label = ", ".join(f"{k}={v}" for k, v in s["metric"].items() if k != "__name__") or "valor"
        if args.step_plot:
            ax.step(xs, ys, where="post", label=label, linewidth=1.8)
        else:
            ax.plot(xs, ys, label=label, linewidth=1.8)

    if args.yticks:
        ticks = [item.split("=", 1) for item in args.yticks.split(",")]
        ax.set_yticks([float(v) for v, _ in ticks], [name for _, name in ticks])
    if args.ymax is not None:
        ax.set_ylim(0, args.ymax)
    ax.set_ylabel(args.unit)
    ax.xaxis.set_major_formatter(mdates.DateFormatter("%H:%M:%S"))
    ax.grid(alpha=0.3)
    if series:
        ax.legend(fontsize=8, loc="upper left")
    window = (f"{datetime.fromtimestamp(start):%Y-%m-%d %H:%M:%S} → {datetime.fromtimestamp(end):%H:%M:%S} "
              f"(horario local) · step {args.step}")
    ax.set_title(f"{args.title}\n{window}", fontsize=10)
    fig.text(0.01, 0.01, "PromQL: " + args.query, fontsize=7, family="monospace", wrap=True)
    fig.tight_layout(rect=(0, 0.06, 1, 1))
    fig.savefig(args.out)
    print(f"{args.out}: {len(series)} serie(s)")


if __name__ == "__main__":
    main()
