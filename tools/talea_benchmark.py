#!/usr/bin/env python3
"""Control-node, bounded GSM8K traffic generator; Python standard library only."""
import argparse
import base64
import concurrent.futures
import datetime
import hashlib
import json
import os
import random
import statistics
import sys
import time
import urllib.request
from pathlib import Path

REVISION = "3101c7d5072418e28b9008a6636bde82a006892c"
DATASET_PATH = "grade_school_math/data/test.jsonl"
DATASET_URL = "https://raw.githubusercontent.com/openai/grade-school-math/" + REVISION + "/" + DATASET_PATH
DATASET_API = "https://api.github.com/repos/openai/grade-school-math/contents/" + DATASET_PATH + "?ref=" + REVISION
DATASET_SHA256 = "3730d312f6e3440559ace48831e51066acaca737f6eabec99bccb9e4b3c39d14"
# The pinned Git commit fixes provenance; the content digest is recorded in every report.


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, hdrs, newurl):
        return None


def open_request(request, timeout=120):
    return urllib.request.build_opener(NoRedirect).open(request, timeout=timeout)


def dataset(root):
    path = root / "datasets/gsm8k-test.jsonl"
    manifest = path.with_suffix(".manifest.json")
    path.parent.mkdir(parents=True, exist_ok=True)
    if not path.exists():
        try:
            with open_request(DATASET_URL, 20) as response:
                raw = response.read(4 << 20)
        except Exception:
            with open_request(DATASET_API, 30) as response:
                data = json.load(response)
            raw = base64.b64decode(data["content"], validate=False)
        path.with_suffix(".part").write_bytes(raw)
        if hashlib.sha256(raw).hexdigest() != DATASET_SHA256:
            raise ValueError("downloaded dataset digest mismatch")
        path.with_suffix(".part").replace(path)
    if not manifest.exists():
        raw = path.read_bytes()
        rows = [json.loads(line) for line in raw.splitlines() if line]
        if len(rows) != 1319 or any(not isinstance(row.get("question"), str) for row in rows):
            raise ValueError("GSM8K dataset validation failed")
        digest = hashlib.sha256(raw).hexdigest()
        info = dict(name="GSM8K test", revision=REVISION, url=DATASET_URL, sha256=digest,
                    license="MIT", license_url="https://github.com/openai/grade-school-math/blob/"+REVISION+"/LICENSE")
        manifest.write_text(json.dumps(info, indent=2), encoding="utf-8")
    raw = path.read_bytes()
    info = json.loads(manifest.read_text())
    if info.get("revision") != REVISION or hashlib.sha256(raw).hexdigest() != DATASET_SHA256 or info["sha256"] != DATASET_SHA256:
        raise ValueError("cached dataset digest/revision mismatch")
    return [json.loads(line)["question"] for line in raw.splitlines() if line], info


def request_one(base_url, model, prompt, max_tokens, timeout, api_key="", ignore_eos=False):
    payload = dict(model=model, messages=[dict(role="user", content=prompt)], max_tokens=max_tokens,
                   temperature=0, stream=True, stream_options=dict(include_usage=True))
    if ignore_eos:
        payload["ignore_eos"] = True
    headers = {"Content-Type": "application/json"}
    if api_key:
        headers["Authorization"] = "Bearer " + api_key
    req = urllib.request.Request(base_url.rstrip("/")+"/v1/chat/completions",
                                 data=json.dumps(payload).encode(), headers=headers)
    start = time.perf_counter()
    deadline = time.monotonic() + timeout
    first = last = None
    usage = None
    done = False
    try:
        with open_request(req, timeout) as response:
            for raw in response:
                if time.monotonic() > deadline:
                    raise TimeoutError("request exceeded total deadline")
                if not raw.startswith(b"data:"):
                    continue
                data = raw[5:].strip()
                if data == b"[DONE]":
                    done = True
                    break
                chunk = json.loads(data)
                if chunk.get("error"):
                    raise ValueError("inference error in stream")
                if chunk.get("usage"):
                    usage = chunk["usage"]
                choices = chunk.get("choices") or []
                if choices and choices[0].get("delta", {}).get("content"):
                    last = time.perf_counter()
                    if first is None:
                        first = last
        elapsed = time.perf_counter() - start
        if not done or first is None:
            raise ValueError("incomplete or empty stream")
        # Never approximate tokens using characters or SSE chunk counts.
        if not usage or not isinstance(usage.get("prompt_tokens"), int) or not isinstance(usage.get("completion_tokens"), int):
            raise ValueError("stream has no actual token usage")
        output = usage["completion_tokens"]
        return dict(ok=True, latency_s=elapsed, ttft_s=first-start,
                    tpot_s=(last-first)/(output-1) if output>1 else None,
                    prompt_tokens=usage["prompt_tokens"], completion_tokens=output)
    except Exception as error:
        # Do not persist response content, credentials or prompts in reports.
        return dict(ok=False, latency_s=time.perf_counter()-start, error=type(error).__name__+": "+str(error)[:200])


def percentile(values, q):
    if not values:
        return None
    values = sorted(values)
    pos = (len(values)-1)*q
    lo = int(pos)
    return values[lo]+(values[min(lo+1, len(values)-1)]-values[lo])*(pos-lo)


def summarize(results, seconds):
    success = [r for r in results if r["ok"]]
    output = sum(r["completion_tokens"] for r in success)
    inputs = sum(r["prompt_tokens"] for r in success)
    report = dict(requests=len(results), successes=len(success), failures=len(results)-len(success), elapsed_s=seconds,
                  requests_per_second=len(success)/seconds, input_tokens=inputs, output_tokens=output,
                  input_tokens_per_second=inputs/seconds, output_tokens_per_second=output/seconds)
    for key in ("latency_s", "ttft_s", "tpot_s"):
        samples = [r[key]*1000 for r in success if r[key] is not None]
        report[key.replace("_s", "_ms")] = dict(mean=statistics.mean(samples) if samples else None,
                                                  p50=percentile(samples, .5), p95=percentile(samples, .95))
    return report


def main(argv=None, config=None):
    parser = argparse.ArgumentParser(description="在控制节点下载 GSM8K 并向 Router 发请求；Ctrl+C 停止")
    parser.add_argument("--requests", type=int, default=64)
    parser.add_argument("--concurrency", type=int, default=4)
    parser.add_argument("--max-tokens", type=int, default=256)
    parser.add_argument("--warmup", type=int, default=2)
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--timeout", type=int, default=120)
    parser.add_argument("--ignore-eos", action="store_true", help="固定输出长度压力测试（报告会注明）")
    parser.add_argument("--base-url")
    parser.add_argument("--model")
    parser.add_argument("--output-dir", default="/opt/tai-talea/benchmarks")
    args = parser.parse_args(argv)
    for name, low, high in [("requests",1,10000),("concurrency",1,64),("max_tokens",1,4096),("warmup",0,20),("timeout",1,300)]:
        if not low<=getattr(args,name)<=high:
            parser.error(f"{name}: allowed range {low}..{high}")
    config = config or {}
    base_url = args.base_url or config.get("router_url")
    model = args.model or config.get("model")
    if not base_url or not model:
        parser.error("需要客户端配置 router_url 和 model，或 --base-url / --model")
    root = Path(args.output_dir)
    root.mkdir(parents=True, exist_ok=True)
    # Only one pressure test per output directory; no orphan child workload.
    lock = root / "active.lock"
    try:
        fd = os.open(lock, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except FileExistsError:
        raise RuntimeError("已有 benchmark 在运行；若上次异常终止，请确认 PID 已退出后删除 " + str(lock)) from None
    with os.fdopen(fd,"w") as output:
        output.write(str(os.getpid()))
    try:
        questions, source = dataset(root)
        rng = random.Random(args.seed)
        rng.shuffle(questions)
        key = os.environ.get("TALEA_INFERENCE_API_KEY", "")
        call = lambda q: request_one(base_url, model, q, args.max_tokens, args.timeout, key, args.ignore_eos)
        print(f"数据集 {source['name']} · 请求 {args.requests} · 并发 {args.concurrency} · 输出上限 {args.max_tokens}", flush=True)
        for idx in range(args.warmup):
            warmup = call(questions[-1-idx])
            if not warmup["ok"]:
                raise RuntimeError("warmup failed; Router / PD service is not ready: " + warmup["error"])
        stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
        start = time.perf_counter()
        results = []
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as pool:
            # Bounded producer: at most concurrency requests are pending, even
            # for long runs, and interruption never submits the rest of the run.
            pending = set()
            submitted = 0
            try:
                while submitted<args.requests or pending:
                    while submitted<args.requests and len(pending)<args.concurrency:
                        pending.add(pool.submit(call, questions[submitted % len(questions)]))
                        submitted += 1
                    finished, pending = concurrent.futures.wait(pending, return_when=concurrent.futures.FIRST_COMPLETED)
                    results.extend(f.result() for f in finished)
                    print(f"完成 {len(results)}/{args.requests} · 失败 {sum(not r['ok'] for r in results)}", flush=True)
            except KeyboardInterrupt:
                for future in pending:
                    future.cancel()
                raise
        report = dict(started_at=stamp, dataset=source, parameters=vars(args), model=model,
                      summary=summarize(results,time.perf_counter()-start), results=results,
                      notes=["closed-loop concurrency; warmup excluded", "TPOT is per-request average after first content, not inter-chunk p95", "input tokens include prefix cache hits; this is not an accuracy evaluation"])
        path = root / ("gsm8k-"+stamp+".json")
        path.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
        print(json.dumps(report["summary"], ensure_ascii=False, indent=2))
        print("报告：" + str(path), flush=True)
        return 0 if not report["summary"]["failures"] else 1
    finally:
        lock.unlink(missing_ok=True)


if __name__ == "__main__":
    sys.exit(main())
