"""LongMemEval bench adaptado pra NEXUS v2 — Sprint 1 (2026-05-21).

Patches sobre versão original (2026-05-20):
- [1.1] Fix `gold[:120]` → `str(gold)[:120]` (gold pode ser int)
- [1.2] Judges per-type: PREFERENCE + TEMPORAL dedicados; fallback genérico
- [1.3-bench] Injeta [Session date: YYYY/MM/DD] no header de cada session
- [1.3-bench] Injeta [Today: YYYY/MM/DD] no prefixo da query
- Usa haystack_dates + question_date do dataset oracle

Uso:
  NEXUS_V2_TOKEN=<token> \
  ANTHROPIC_API_KEY=$(op read '<your-secret-manager-path>) \
  LME_SAMPLE=10 \
  python3 /tmp/lme_nexus_v2.py
"""
from __future__ import annotations
import asyncio, json, os, random, re, sys, time, uuid, subprocess
from collections import Counter, defaultdict
import httpx

API         = os.getenv("LME_API", "http://127.0.0.1:8044")
TOKEN       = os.getenv("NEXUS_V2_TOKEN") or sys.exit("set NEXUS_V2_TOKEN")
DATA        = os.getenv("LME_DATA", "/tmp/LongMemEval/data/longmemeval_oracle.json")
N           = int(os.getenv("LME_SAMPLE", "10"))
LIMIT       = int(os.getenv("LME_LIMIT", "5"))
SEED        = int(os.getenv("LME_SEED", "42"))
MODE        = os.getenv("LME_MODE", "hybrid")
PG_CONT     = os.getenv("LME_PG_CONTAINER", "nexus-v2-postgres")
JUDGE_MODEL = os.getenv("LME_JUDGE_MODEL", "claude-sonnet-4-6")

# Sprint 1.2 — judges per-type (padrão mnemonexus que NEXUS v2 perdeu na port)
JUDGE_GENERIC = """I will give you a question, a correct answer, and a response from a model. Please answer yes if the response contains the correct answer. Otherwise answer no.

LENIENCY RULES:
- If the correct answer is "Yes"/"No" and the response clearly affirms or denies (even with extra explanatory detail), answer yes.
- If the response contains the correct answer or equivalent intermediate steps, answer yes.
- Minor differences in unit/phrasing are OK ("5 trips" vs "five trips"; "3 months" vs "three months ago").
- For multi-part questions, accept the response if it provides the most important value(s) requested.
- Only answer no if the response is factually wrong, missing the key value, or only a guess/hypothesis.

Question: {question}
Correct Answer: {answer}
Model Response: {response}

Is the model response correct? Answer yes or no only."""

JUDGE_PREFERENCE = """I will give you a question about a user's preferences/likes/dislikes, the correct answer (which represents the user's actual preferences), and a model's response. Please answer yes if the model's response captures the same key preferences as the correct answer, even if phrased differently or in different order. Answer yes if the model includes the main preferred items/categories mentioned in the correct answer; minor differences in wording or extra valid suggestions consistent with the user's taste are OK. Answer no if the model recommends something CONTRADICTORY to the user's stated preferences, or misses the core preference. Answer no for generic recommendations that ignore the user's specific tastes.

Question: {question}
Correct Answer: {answer}
Model Response: {response}

Is the model response correct? Answer yes or no only."""

JUDGE_TEMPORAL = """I will give you a question about temporal reasoning (dates, durations, ordering of events), the correct answer, and a model's response. Please answer yes if the response contains the correct date, duration, or temporal relationship — even if phrased differently. Accept equivalent formulations: "3 months" == "90 days" approximately, "March 2024" == "03/2024" == "in March 2024", "2 years older" == "two years older". Accept if the response shows the correct intermediate steps (e.g. computing date deltas) even if final wording differs slightly. Answer no if the response is wrong, missing, or only a guess.

Question: {question}
Correct Answer: {answer}
Model Response: {response}

Is the model response correct? Answer yes or no only."""

JUDGE_MULTISESSION = """I will give you a question that requires aggregating, counting, or comparing facts spread across multiple chat sessions, the correct answer, and a model's response. Answer yes or no.

ACCEPT (yes) when:
- The response states the correct final value, even with qualifiers like "about", "around", "approximately", "over", "~", or "at least" — e.g. "over $270 more" matches a gold of "$270"; "around 140 hours" matches "140 hours".
- A numeric answer is within ~5% of the gold value (rounding or unit differences only).
- The response shows the correct component values AND the arithmetic to the gold total (e.g. correctly sums the parts).
- The gold answer indicates the information is INSUFFICIENT or that something was NOT mentioned, AND the response also declines to answer or correctly identifies that the item/data is missing or absent — even while reporting the related information it does have. Do not require identical wording.

REJECT (no) when:
- The response under-counts or over-counts discrete items (e.g. gold 3, response 2; gold 4, response 3).
- A numeric answer differs from the gold by more than ~5% (e.g. gold 140, response 70).
- The response FABRICATES a specific value when the gold says the information is insufficient.
- The response picks the wrong entity in a superlative or comparison (e.g. the wrong store for "where I spent the most").
- The key value is wrong, missing, or only a guess.

Question: {question}
Correct Answer: {answer}
Model Response: {response}

Is the model response correct? Answer yes or no only."""


def judge_prompt_for(qtype: str) -> str:
    if "preference" in qtype:
        return JUDGE_PREFERENCE
    if "temporal" in qtype:
        return JUDGE_TEMPORAL
    if "multi-session" in qtype:
        return JUDGE_MULTISESSION
    return JUDGE_GENERIC


def normalize_date(raw: str) -> str:
    """'2023/04/10 (Mon) 17:50' → '2023/04/10'. Tolerante a formatos."""
    if not raw:
        return ""
    m = re.match(r"^\s*(\d{4})[/\-](\d{1,2})[/\-](\d{1,2})", str(raw))
    if not m:
        return ""
    return f"{m.group(1)}/{int(m.group(2)):02d}/{int(m.group(3)):02d}"


def stratified_sample(items, n, key):
    by = defaultdict(list)
    for it in items:
        by[key(it)].append(it)
    sample = []
    per = max(1, n // len(by))
    for k, v in by.items():
        random.shuffle(v)
        sample.extend(v[:per])
    random.shuffle(sample)
    return sample[:n]


def session_text(session, max_chars: int = 25000, date_header: str = ""):
    """Sprint 1.3 — prepend [Session date: YYYY/MM/DD] no início do content.
    Permite Go SortByDateDesc parsear e ordenar chunks cronologicamente."""
    body = "\n".join(f"{m['role']}: {m['content']}" for m in session)
    if date_header:
        body = f"[Session date: {date_header}]\n{body}"
    return body[:max_chars]


def db_cleanup():
    """TRUNCATE pages CASCADE."""
    subprocess.run([
        "docker", "exec", PG_CONT, "psql", "-U", "nexus", "-d", "nexus",
        "-c", "TRUNCATE pages, chunks, entities, edges, sessions, messages, river_job, user_profiles RESTART IDENTITY CASCADE;",
    ], capture_output=True, check=False)


def db_count_embedded() -> int:
    r = subprocess.run([
        "docker", "exec", PG_CONT, "psql", "-U", "nexus", "-d", "nexus",
        "-tAc", "SELECT count(*) FROM pages p WHERE NOT EXISTS (SELECT 1 FROM chunks c WHERE c.page_id = p.id AND c.embedding IS NULL);",
    ], capture_output=True, text=True, check=False)
    try:
        return int(r.stdout.strip())
    except ValueError:
        return 0


def trigger_profile_rebuild(org_id: int = 1, timeout_s: int = 60):
    """Sprint 3.1 patch — enfileira BuildUserProfileJob síncrono.

    Sem isso, PeriodicJob (1h) nunca dispara durante o bench, profile fica
    vazio, e LoadProfile no /v1/query retorna empty → preference cai pra
    baseline pré-3.1. Patch força build após ingest e antes de query.
    """
    enqueue_sql = (
        "INSERT INTO river_job (state, attempt, max_attempts, args, kind, priority, queue, metadata) "
        f"VALUES ('available', 0, 3, '{{\"organization_id\": {org_id}}}'::jsonb, "
        "'build_user_profile', 1, 'default', '{}'::jsonb) RETURNING id"
    )
    r = subprocess.run([
        "docker", "exec", PG_CONT, "psql", "-U", "nexus", "-d", "nexus", "-tAc", enqueue_sql,
    ], capture_output=True, text=True, check=False)
    job_id = r.stdout.strip()
    if not job_id.isdigit():
        return
    for _ in range(timeout_s):
        r = subprocess.run([
            "docker", "exec", PG_CONT, "psql", "-U", "nexus", "-d", "nexus", "-tAc",
            f"SELECT state FROM river_job WHERE id={job_id}",
        ], capture_output=True, text=True, check=False)
        if r.stdout.strip() in ("completed", "discarded", "cancelled"):
            return
        time.sleep(1)


JUDGE_PROVIDER = os.getenv("LME_JUDGE_PROVIDER", "anthropic")
JUDGE_GEMINI_MODEL = os.getenv("LME_JUDGE_GEMINI_MODEL", "gemini-3.5-flash")


async def judge_with(client: httpx.AsyncClient, template: str, question: str, gold, response: str) -> bool:
    prompt = template.format(question=question, answer=str(gold), response=response)
    if JUDGE_PROVIDER == "openrouter":
        key = os.environ["OPENROUTER_API_KEY"]
        r = await client.post(
            "https://openrouter.ai/api/v1/chat/completions",
            headers={"Authorization": f"Bearer {key}", "content-type": "application/json"},
            json={"model": os.getenv("LME_JUDGE_OR_MODEL", "google/gemini-2.5-flash"),
                  "max_tokens": 24, "temperature": 0, "reasoning": {"enabled": False},
                  "messages": [{"role": "user", "content": prompt}]},
            timeout=40,
        )
        if r.status_code >= 400: raise RuntimeError(f"HTTP {r.status_code}: {r.text[:500]}")
        msg = r.json()["choices"][0]["message"]
        text = (msg.get("content") or "").strip().lower()
        return text.startswith("yes")
    if JUDGE_PROVIDER == "gemini":
        key = os.environ["GEMINI_API_KEY"]
        r = await client.post(
            f"https://generativelanguage.googleapis.com/v1beta/models/{JUDGE_GEMINI_MODEL}:generateContent?key={key}",
            json={"contents": [{"parts": [{"text": prompt}]}],
                  "generationConfig": {"thinkingConfig": {"thinkingBudget": 0}, "maxOutputTokens": 16, "temperature": 0}},
            timeout=40,
        )
        if r.status_code >= 400: raise RuntimeError(f"HTTP {r.status_code}: {r.text[:500]}")
        cands = r.json().get("candidates")
        if not cands: raise RuntimeError(f"gemini judge no candidates: {r.text[:300]}")
        text = cands[0]["content"]["parts"][0]["text"].strip().lower()
        return text.startswith("yes")
    key = os.environ["ANTHROPIC_API_KEY"]
    r = await client.post(
        "https://api.anthropic.com/v1/messages",
        headers={"x-api-key": key, "anthropic-version": "2023-06-01", "content-type": "application/json"},
        json={"model": JUDGE_MODEL, "max_tokens": 8, "messages": [{"role": "user", "content": prompt}]},
        timeout=30,
    )
    if r.status_code >= 400: raise RuntimeError(f"HTTP {r.status_code}: {r.text[:500]}")
    text = r.json()["content"][0]["text"].strip().lower()
    return text.startswith("yes")


async def judge_anthropic(client: httpx.AsyncClient, qtype: str, question: str, gold, response: str) -> bool:
    return await judge_with(client, judge_prompt_for(qtype), question, gold, response)


# ── Painel de juízes (anti self-preference) ───────────────────────────────────
# Julga a MESMA resposta com N modelos de FAMÍLIAS distintas (via OpenRouter) e
# decide por MAIORIA. Neutraliza o viés de auto-preferência (um juiz favorece a
# própria família) — sem isso, comparar geradores de famílias diferentes é injusto:
# foi o que inflou Gemini-gen + Gemini-juiz a 90,6%. Reusa OPENROUTER_API_KEY.
# Ative com LME_JUDGE_PANEL=1; modelos via LME_PANEL_MODELS (csv família/modelo).
JUDGE_PANEL = os.getenv("LME_JUDGE_PANEL", "") in ("1", "true", "on", "yes")
PANEL_MODELS = [m.strip() for m in os.getenv(
    "LME_PANEL_MODELS",
    "openai/gpt-4o-mini,google/gemini-2.5-flash,anthropic/claude-haiku-4.5",
).split(",") if m.strip()]


async def _judge_or_model(client: httpx.AsyncClient, model: str, prompt: str) -> bool:
    """Um voto de juiz via OpenRouter (qualquer família)."""
    key = os.environ["OPENROUTER_API_KEY"]
    r = await client.post(
        "https://openrouter.ai/api/v1/chat/completions",
        headers={"Authorization": f"Bearer {key}", "content-type": "application/json"},
        json={"model": model, "max_tokens": 24, "temperature": 0,
              "reasoning": {"enabled": False},
              "messages": [{"role": "user", "content": prompt}]},
        timeout=40,
    )
    if r.status_code >= 400:
        raise RuntimeError(f"HTTP {r.status_code} ({model}): {r.text[:300]}")
    text = (r.json()["choices"][0]["message"].get("content") or "").strip().lower()
    return text.startswith("yes")


async def judge_panel(client: httpx.AsyncClient, qtype: str, question: str, gold, response: str):
    """Veredito por MAIORIA de PANEL_MODELS. Retorna (correto, votos:list[bool]).
    Tolera falha de juízes individuais — decide pelos que responderam (empate=False)."""
    prompt = judge_prompt_for(qtype).format(question=question, answer=str(gold), response=response)
    results = await asyncio.gather(
        *[_judge_or_model(client, m, prompt) for m in PANEL_MODELS],
        return_exceptions=True,
    )
    votes = [v for v in results if isinstance(v, bool)]
    if not votes:
        raise RuntimeError(f"painel: todos os juízes falharam: {results}")
    yes = sum(1 for v in votes if v)
    return (yes > len(votes) / 2), votes


async def ingest_one(client: httpx.AsyncClient, title: str, content: str, metadata: dict):
    r = await client.post(f"{API}/v1/ingest",
        headers={"Authorization": f"Bearer {TOKEN}", "Content-Type": "application/json"},
        json={"title": title, "content": content, "domain": "memory", "metadata": metadata},
        timeout=60)
    if r.status_code >= 400: raise RuntimeError(f"HTTP {r.status_code}: {r.text[:500]}")
    return r.json()


async def query(client: httpx.AsyncClient, question: str, limit: int, mode: str) -> dict:
    transient = ("overloaded", "2062", "2064", "529", "peak-hour", "traffic", "temporarily busy")
    last = ""
    for attempt in range(5):
        r = await client.post(f"{API}/v1/query",
            headers={"Authorization": f"Bearer {TOKEN}", "Content-Type": "application/json"},
            json={"question": question, "limit": limit, "mode": mode, "domain": "memory", "multi_hop": True},
            timeout=120)
        if r.status_code < 400:
            return r.json()
        body = r.text[:500]
        last = f"HTTP {r.status_code}: {body}"
        if r.status_code >= 500 or any(t in body for t in transient):
            await asyncio.sleep(30 * (attempt + 1))
            continue
        raise RuntimeError(last)
    raise RuntimeError(f"esgotou retries: {last}")


async def run_one(client: httpx.AsyncClient, judge_client: httpx.AsyncClient, item):
    """Returns (correct, latency_ms, generated_answer, n_sessions)."""
    tag = uuid.uuid4().hex[:6]

    haystack_dates = item.get("haystack_dates") or []
    expected = len(item["haystack_sessions"])
    # Sprint 1.5.3 — paraleliza ingest (N sessions). Reduz wall-time linear N→1.
    ingest_tasks = []
    for idx, (sid, sess) in enumerate(zip(item["haystack_session_ids"], item["haystack_sessions"])):
        date_iso = normalize_date(haystack_dates[idx]) if idx < len(haystack_dates) else ""
        ingest_tasks.append(ingest_one(
            client,
            f"lme-{tag}-{sid}",
            session_text(sess, date_header=date_iso),
            {"lme_question_id": item["question_id"], "lme_session_id": sid, "lme_tag": tag, "lme_session_date": date_iso},
        ))
    await asyncio.gather(*ingest_tasks)

    # Espera embedding + extract drenarem
    for _ in range(480):
        n = db_count_embedded()
        if n >= expected:
            break
        await asyncio.sleep(1.0)
    else:
        return False, 0, f"TIMEOUT: só {n}/{expected} pages embedded", expected

    # Sprint 3.1 patch — força rebuild síncrono do user_profile pra que
    # /v1/query possa injetar o bloco USER PROFILE. Sem isso, PeriodicJob
    # 1h não dispara dentro da janela do bench e LoadProfile retorna vazio.
    trigger_profile_rebuild(org_id=1, timeout_s=60)

    # Sprint 1.3 — [Today: YYYY/MM/DD] header na pergunta. Padrão mnemonexus.
    today_iso = normalize_date(item.get("question_date") or "")
    qtext = item["question"]
    if today_iso:
        qtext = f"[Today: {today_iso}] {qtext}"

    t0 = time.perf_counter()
    r = await query(client, qtext, LIMIT, MODE)
    lat = (time.perf_counter() - t0) * 1000
    response = (r.get("answer") or "").strip()
    if not response:
        return False, lat, "", expected

    if JUDGE_PANEL:
        correct, _ = await judge_panel(judge_client, item["question_type"], item["question"], item["answer"], response)
    else:
        correct = await judge_anthropic(judge_client, item["question_type"], item["question"], item["answer"], response)
    return correct, lat, response, expected


async def main():
    random.seed(SEED)
    data = json.load(open(DATA))
    # Filtro opcional por tipo (ex: LME_TYPE=multi-session p/ re-bench focado).
    only_type = os.getenv("LME_TYPE")
    if only_type:
        data = [d for d in data if d["question_type"] == only_type]
    # Modo dual-judge: julga a MESMA resposta com o juiz GENÉRICO (antigo) e o
    # type-specific (novo) p/ isolar o efeito do juiz sem variância de geração.
    dual = os.getenv("LME_DUAL_JUDGE") == "1"
    sample = stratified_sample(data, N, key=lambda x: x["question_type"])
    print(f"Loaded {len(data)} items{f' (type={only_type})' if only_type else ''}; sampling {len(sample)} stratified by question_type")
    _judge_label = (f"PANEL[{','.join(PANEL_MODELS)}] majority" if JUDGE_PANEL
                    else f"openrouter/{os.getenv('LME_JUDGE_OR_MODEL','google/gemini-2.5-flash')}" if JUDGE_PROVIDER == "openrouter"
                    else f"gemini/{JUDGE_GEMINI_MODEL}" if JUDGE_PROVIDER == "gemini"
                    else f"anthropic/{JUDGE_MODEL}")
    print(f"Judge: {_judge_label} (per-type: preference/temporal/multi-session dedicated){' [DUAL]' if dual else ''}")
    print(f"Retrieval: mode={MODE} limit={LIMIT}")
    print(f"Types: {dict(Counter(x['question_type'] for x in sample))}")
    print()

    if JUDGE_PANEL:
        if not os.getenv("OPENROUTER_API_KEY"):
            sys.exit("set OPENROUTER_API_KEY (painel usa OpenRouter p/ as 3 famílias)")
    elif JUDGE_PROVIDER == "gemini":
        if not os.getenv("GEMINI_API_KEY"):
            sys.exit("set GEMINI_API_KEY")
    elif not os.getenv("ANTHROPIC_API_KEY"):
        sys.exit("set ANTHROPIC_API_KEY")

    async with httpx.AsyncClient(timeout=180) as client, httpx.AsyncClient() as judge:
        hits = 0
        hits_old = 0          # tally do juiz genérico (dual mode)
        flips_gain = []       # old=MISS, new=OK (falsos-negativos recuperados)
        flips_loss = []       # old=OK, new=MISS (não deveria ocorrer)
        lats = []
        by_type = defaultdict(lambda: [0, 0])
        misses = []
        for i, item in enumerate(sample, 1):
            db_cleanup()
            try:
                correct, lat, resp, n_sess = await run_one(client, judge, item)
                hits += int(correct)
                lats.append(lat)
                qt = item["question_type"]
                by_type[qt][0] += int(correct)
                by_type[qt][1] += 1
                mark = "OK" if correct else "MISS"
                flag = ""
                if dual:
                    old = await judge_with(judge, JUDGE_GENERIC, item["question"], item["answer"], resp)
                    hits_old += int(old)
                    if old and not correct:
                        flips_loss.append((qt, item["question"], item["answer"], resp))
                        flag = "  [FLIP↓ old=OK new=MISS]"
                    elif correct and not old:
                        flips_gain.append((qt, item["question"], item["answer"], resp))
                        flag = "  [FLIP↑ old=MISS new=OK]"
                preview = re.sub(r"\s+", " ", resp)[:80]
                print(f"  [{i:3d}/{len(sample)}] {mark:4s} {qt:28s} sess={n_sess} lat={lat:6.0f}ms  -> {preview!r}{flag}")
                if not correct:
                    misses.append((qt, item["question"], item["answer"], resp))
            except Exception as e:
                print(f"  [{i:3d}] ERROR {item['question_type']}: {e}")
                by_type[item["question_type"]][1] += 1

    print()
    print("=" * 60)
    if dual:
        n = len(lats)
        print(f"[DUAL] Juiz GENÉRICO (antigo): {hits_old}/{n} = {hits_old/max(1,n):.1%}")
        print(f"[DUAL] Juiz NOVO (type-specific): {hits}/{n} = {hits/max(1,n):.1%}")
        print(f"[DUAL] Recuperados (old MISS→new OK): {len(flips_gain)}  |  Perdidos (old OK→new MISS): {len(flips_loss)}")
        for tag, lst in (("RECUPERADO", flips_gain), ("PERDIDO", flips_loss)):
            for qt, q, gold, resp in lst:
                print(f"  [{tag}] {str(q)[:100]}")
                print(f"     gold: {str(gold)[:100]}")
                print(f"     pred: {str(resp)[:240]}")
        print()
    print(f"Accuracy: {hits}/{len(sample)} = {hits/max(1,len(sample)):.1%}")
    if lats:
        lats.sort()
        print(f"Latency p50={lats[len(lats)//2]:.0f}ms  p95={lats[int(len(lats)*0.95)]:.0f}ms")
    print()
    print("By question_type:")
    for qt, (h, t) in sorted(by_type.items()):
        print(f"  {qt:30s} {h:3d}/{t:3d} = {h/max(1,t):.1%}")

    if misses:
        print()
        print(f"--- Misses ({len(misses)}) ---")
        for qt, q, gold, resp in misses:
            # Sprint 1.1 — str() guard pra ints/floats no gold
            print(f"[{qt}] Q: {str(q)[:120]}")
            print(f"  gold: {str(gold)[:120]}")
            print(f"  pred: {str(resp)[:200]}")
            print()


if __name__ == "__main__":
    asyncio.run(main())
