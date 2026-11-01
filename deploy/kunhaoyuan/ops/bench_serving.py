#!/usr/bin/env python3
"""Measure a talea endpoint with lengths that are actually what they claim.

`sglang.bench_serving --dataset-name random-ids` does not honour
`--random-range-ratio`, so `--random-input-len 4096` produced prompts averaging
2200 tokens and `--random-output-len 256` produced replies that stopped at EOS.
A baseline whose lengths vary per request is not a baseline.

This posts `input_ids` directly to `/generate`, so the prompt is exactly N tokens
by construction and needs no tokenizer, and it sets `ignore_eos`, so the reply is
exactly M tokens. Every reported number is then measured against a length that is
known rather than assumed.

usage:
  bench_serving.py --base-url http://talea-router:8000 \
      --concurrency 1 16 64 128 --num-prompts 8 64 256 512 \
      --input-len 4096 --output-len 256
"""

import argparse
import json
import random
import statistics
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor

import requests


def build_prompt(vocab, length, seed):
    rng = random.Random(seed)
    # High ids on purpose: a prompt of low ids would sit in a dense region of
    # the vocabulary and could be more cache-friendly than a real one.
    return [rng.randrange(vocab // 2, vocab - 1) for _ in range(length)]


def one_request(session, url, prompt, max_new, timeout):
    body = {
        "input_ids": prompt,
        "sampling_params": {
            "max_new_tokens": max_new,
            "temperature": 0.0,
            "ignore_eos": True,
        },
        "stream": True,
    }
    start = time.perf_counter()
    ttft = None
    tokens = None
    chunks = 0
    with session.post(url, json=body, stream=True, timeout=timeout) as response:
        response.raise_for_status()
        for line in response.iter_lines():
            if not line:
                continue
            if not line.startswith(b"data:"):
                continue
            payload = line[5:].strip()
            if not payload or payload == b"[DONE]":
                continue
            if ttft is None:
                ttft = time.perf_counter() - start
            chunks += 1
            try:
                obj = json.loads(payload)
            except ValueError:
                continue
            meta = obj.get("meta_info") or {}
            # completion_tokens is authoritative; a chunk is not one token when
            # speculative decoding accepts several.
            if "completion_tokens" in meta:
                tokens = meta["completion_tokens"]
    end = time.perf_counter()
    if ttft is None:
        raise RuntimeError("the stream produced no data frame")
    if tokens is None:
        tokens = chunks
    return {
        "ttft": ttft,
        "total": end - start,
        "tokens": tokens,
        "chunks": chunks,
    }


def run_stage(base_url, model, concurrency, num_prompts, input_len, output_len, vocab, timeout, seed):
    prompt_seed = build_prompt(vocab, 8, seed)  # warm the token id range only
    del prompt_seed
    url = base_url.rstrip("/") + "/generate"
    local = threading.local()

    def session_for_thread():
        s = getattr(local, "session", None)
        if s is None:
            s = requests.Session()
            s.headers.update({"Content-Type": "application/json"})
            local.session = s
        return s

    prompts = [build_prompt(vocab, input_len, seed + i) for i in range(min(num_prompts, 8))]

    results = []
    errors = []

    def task(index):
        prompt = prompts[index % len(prompts)]
        try:
            return one_request(session_for_thread(), url, prompt, output_len, timeout)
        except Exception as exc:  # noqa: BLE001 - a failed request is data too
            errors.append(f"{type(exc).__name__}: {exc}")
            return None

    started = time.perf_counter()
    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        for outcome in pool.map(task, range(num_prompts)):
            if outcome is not None:
                results.append(outcome)
    wall = time.perf_counter() - started

    if not results:
        return {"concurrency": concurrency, "failed": len(errors), "error": errors[:3]}

    ttfts = sorted(r["ttft"] for r in results)
    e2es = sorted(r["total"] for r in results)
    total_tokens = sum(r["tokens"] for r in results)
    decode = sum(max(r["total"] - r["ttft"], 0) for r in results)
    itls = []
    for r in results:
        steps = max(r["tokens"] - 1, 1)
        itls.append(max(r["total"] - r["ttft"], 0) / steps)

    def pct(values, p):
        if not values:
            return 0.0
        k = min(len(values) - 1, int(round((p / 100.0) * (len(values) - 1))))
        return values[k]

    return {
        "concurrency": concurrency,
        "requested": num_prompts,
        "ok": len(results),
        "failed": len(errors),
        "error": errors[:3],
        "wall_s": round(wall, 2),
        "req_per_s": round(len(results) / wall, 3),
        "input_tokens": input_len,
        "output_tokens": output_len,
        "total_out_tokens": total_tokens,
        "out_tok_per_s": round(total_tokens / wall, 1),
        "in_tok_per_s": round(len(results) * input_len / wall, 1),
        "ttft_ms_mean": round(statistics.mean(ttfts) * 1000, 1),
        "ttft_ms_p50": round(pct(ttfts, 50) * 1000, 1),
        "ttft_ms_p95": round(pct(ttfts, 95) * 1000, 1),
        "tpot_ms_mean": round(statistics.mean(itls) * 1000, 2),
        "tpot_ms_p95": round(pct(sorted(itls), 95) * 1000, 2),
        "e2e_ms_mean": round(statistics.mean(e2es) * 1000, 1),
        "e2e_ms_p95": round(pct(e2es, 95) * 1000, 1),
        "decode_busy_ratio": round(decode / (wall * max(1, min(concurrency, len(results)))), 3),
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default="http://talea-router:8000")
    ap.add_argument("--model", default="glm-5.3-flash")
    ap.add_argument("--concurrency", type=int, nargs="+", default=[1, 16, 64, 128])
    ap.add_argument("--num-prompts", type=int, nargs="+", default=[8, 64, 256, 512])
    ap.add_argument("--input-len", type=int, default=4096)
    ap.add_argument("--output-len", type=int, default=256)
    ap.add_argument("--vocab", type=int, default=154880)
    ap.add_argument("--timeout", type=float, default=600.0)
    ap.add_argument("--seed", type=int, default=42)
    ap.add_argument("--tag", default="run")
    ap.add_argument("--json-out", default="")
    args = ap.parse_args()

    if len(args.num_prompts) == 1:
        args.num_prompts = args.num_prompts * len(args.concurrency)
    if len(args.concurrency) != len(args.num_prompts):
        print("concurrency and num-prompts must have the same length", file=sys.stderr)
        return 2

    print(f"===== BENCH tag={args.tag} base={args.base_url} "
          f"in={args.input_len} out={args.output_len} model={args.model} =====")
    stages = []
    for c, n in zip(args.concurrency, args.num_prompts):
        stage = run_stage(args.base_url, args.model, c, n, args.input_len,
                          args.output_len, args.vocab, args.timeout, args.seed)
        stages.append(stage)
        print(f"\n########## {args.tag} concurrency={c} prompts={n} ##########")
        for key, value in stage.items():
            if key == "error" and not value:
                continue
            print(f"  {key:20s} {value}")

    if args.json_out:
        with open(args.json_out, "w", encoding="utf-8") as fh:
            json.dump({"tag": args.tag, "base_url": args.base_url,
                       "input_len": args.input_len, "output_len": args.output_len,
                       "stages": stages}, fh, indent=2)
        print(f"\nwrote {args.json_out}")
    print("\n===== BENCH DONE =====")
    return 0


if __name__ == "__main__":
    sys.exit(main())
