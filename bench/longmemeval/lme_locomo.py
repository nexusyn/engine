#!/usr/bin/env python3
"""LoCoMo harness para o Nexusyn — reusa ingest/query/judge do lme_nexus_v3.
Ingere cada conversa UMA vez (todas as sessões) e roda as QA contra ela.
Env: LOCOMO_DATA, LOCOMO_SAMPLE (nº conversas), LOCOMO_MAX_QA (0=todas) + as do lme_nexus_v3."""
import os
import sys
import json
import asyncio
from collections import defaultdict

import httpx

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from lme_bench import (  # noqa: E402
    LIMIT, MODE,
    ingest_one, query, db_cleanup, db_count_embedded, trigger_profile_rebuild,
    judge_with, JUDGE_GENERIC, JUDGE_TEMPORAL,
)

DATA = os.getenv("LOCOMO_DATA", "/docker/nexus-bench/locomo10.json")
NSAMP = int(os.getenv("LOCOMO_SAMPLE", "10"))
MAXQA = int(os.getenv("LOCOMO_MAX_QA", "0"))  # 0 = todas as QA da conversa
CAT = {1: "multi-hop", 2: "temporal", 3: "open-domain", 4: "single-hop", 5: "adversarial"}


def session_blocks(conv):
    out, i = [], 1
    while f"session_{i}" in conv:
        turns = conv.get(f"session_{i}") or []
        date = conv.get(f"session_{i}_date_time", "")
        lines = [f"{t.get('speaker', '?')}: {t['text']}" for t in turns if t.get("text")]
        if lines:
            header = f"[Session date: {date}]\n" if date else ""
            out.append((f"session_{i}", header + "\n".join(lines)))
        i += 1
    return out


def judge_tpl(cat):
    return JUDGE_TEMPORAL if cat == 2 else JUDGE_GENERIC


async def run():
    data = json.load(open(DATA))[:NSAMP]
    total_qa = sum(len(s["qa"][:MAXQA] if MAXQA else s["qa"]) for s in data)
    print(f"LoCoMo: {len(data)} conversas, {total_qa} QA | limit={LIMIT} mode={MODE}", flush=True)
    hits = tot = 0
    by_cat = defaultdict(lambda: [0, 0])
    async with httpx.AsyncClient(timeout=180) as client, httpx.AsyncClient() as judge:
        for si, sample in enumerate(data, 1):
            db_cleanup()
            blocks = session_blocks(sample["conversation"])
            await asyncio.gather(*[
                ingest_one(client, f"{sample['sample_id']}-{sk}", text,
                           {"locomo_sample": sample["sample_id"], "session": sk})
                for sk, text in blocks
            ])
            for _ in range(900):
                if db_count_embedded() >= len(blocks):
                    break
                await asyncio.sleep(1.0)
            trigger_profile_rebuild(org_id=1, timeout_s=60)
            qas = sample["qa"][:MAXQA] if MAXQA else sample["qa"]
            for qa in qas:
                q = qa.get("question", "")
                gold = qa.get("answer", "")
                cat = qa.get("category", 0)
                if (gold is None or gold == "") and cat == 5:
                    gold = "No information / not mentioned in the conversation"
                try:
                    r = await query(client, q, LIMIT, MODE)
                    resp = (r.get("answer") or "").strip()
                except Exception:
                    resp = ""
                ok = await judge_with(judge, judge_tpl(cat), q, str(gold), resp) if resp else False
                hits += int(ok)
                tot += 1
                by_cat[cat][0] += int(ok)
                by_cat[cat][1] += 1
                print(f"[{si}/{len(data)} qa{tot}/{total_qa}] {'OK' if ok else 'MISS'} "
                      f"cat{cat}:{CAT.get(cat, '?'):11s} -> {resp[:55]!r}", flush=True)
            print(f"--- {sample['sample_id']}: parcial {hits}/{tot} = {hits / max(1, tot):.1%}", flush=True)
    print("=" * 55)
    print(f"Accuracy: {hits}/{tot} = {hits / max(1, tot):.1%}")
    for c in sorted(by_cat):
        h, t = by_cat[c]
        print(f"  cat{c} {CAT.get(c, '?'):12s} {h}/{t} = {h / max(1, t):.1%}")


if __name__ == "__main__":
    asyncio.run(run())
