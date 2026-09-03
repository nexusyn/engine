#!/usr/bin/env python3
"""Replicação do bench LongMemEval — o fix de medição.

Roda a MESMA config N vezes (mesma seed = mesma amostra) e reporta média ± desvio
por categoria. Isola o ruído de GERAÇÃO: MiniMax-M2.7 é MoE + thinking, não-
determinístico no servidor mesmo a temp 0 — duas rodadas idênticas divergem em
~8-11 casos em n=133. Logo, um A/B de run ÚNICO não mede mudanças de poucos casos.

Uso: mesma env do harness (LME_TYPE, LME_SAMPLE, chaves, NEXUS_V2_TOKEN, ...) +
  LME_REPEAT  (N rodadas, default 3)
  LME_REP_TAG (prefixo dos logs, default 'rep')
  LME_HARNESS (caminho do harness, default ./lme_bench.py)

Cada rodada re-ingere+consulta+julga (custo real). Para A/B, rode duas vezes (config
A e config B) e compare as médias: a mudança só é real se |médiaA - médiaB| superar
a banda de ruído (≈ desvio observado aqui).
"""
import os
import re
import sys
import subprocess
import statistics
from collections import defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))
HARNESS = os.getenv("LME_HARNESS", os.path.join(HERE, "lme_bench.py"))
N = int(os.getenv("LME_REPEAT", "3"))
TAG = os.getenv("LME_REP_TAG", "rep")

acc_re = re.compile(r"^Accuracy:\s+(\d+)/(\d+)")
type_re = re.compile(r"^\s+([A-Za-z][\w-]+)\s+(\d+)/\s*(\d+)\s+=")


def parse(text):
    glob, bytype = None, {}
    for line in text.splitlines():
        m = acc_re.match(line)
        if m:
            glob = (int(m.group(1)), int(m.group(2)))
        m = type_re.match(line)
        if m:
            bytype[m.group(1)] = (int(m.group(2)), int(m.group(3)))
    return glob, bytype


def fmt(vals, tot):
    mean = statistics.mean(vals)
    sd = statistics.pstdev(vals) if len(vals) > 1 else 0.0
    return (f"{mean:5.1f}±{sd:.1f}/{tot}  ({100*mean/tot:5.1f}% ± {100*sd/max(1,tot):.1f}pp)"
            f"  [min {min(vals)} max {max(vals)}]")


def main():
    runs = []
    for i in range(1, N + 1):
        log = f"n_{TAG}_{i}.log"
        print(f"[replicate] run {i}/{N} -> {log}", flush=True)
        with open(log, "w") as f:
            subprocess.run([sys.executable, "-u", HARNESS], stdout=f,
                           stderr=subprocess.STDOUT, env=os.environ.copy())
        glob, bytype = parse(open(log).read())
        if glob is None:
            print(f"[replicate] WARN: run {i} sem Accuracy — ver {log}", flush=True)
            continue
        runs.append((glob, bytype))
        print(f"[replicate] run {i}: {glob[0]}/{glob[1]} = {100*glob[0]/glob[1]:.1f}%", flush=True)

    if not runs:
        sys.exit("[replicate] nenhum run válido")

    cats, totals = defaultdict(list), {}
    for _, bytype in runs:
        for t, (c, tot) in bytype.items():
            cats[t].append(c)
            totals[t] = tot
    gvals = [g[0] for g, _ in runs]
    gtot = runs[0][0][1]

    print("\n" + "=" * 70)
    print(f"REPLICAÇÃO N={len(runs)} (mesma amostra) — média ± desvio dos acertos")
    print("=" * 70)
    for t in sorted(cats):
        print(f"  {t:28s} {fmt(cats[t], totals[t])}")
    print(f"  {'GLOBAL':28s} {fmt(gvals, gtot)}")
    band = max(gvals) - min(gvals)
    print(f"\nBanda de ruído observada (global): {band} casos entre {len(runs)} runs idênticos.")
    print("Regra: uma mudança só é REAL se a diferença de médias superar essa banda.")


if __name__ == "__main__":
    main()
