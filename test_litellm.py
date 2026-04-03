#!/usr/bin/env python3
"""LiteLLM Proxy TTFT Test — OpenAI compatible endpoint"""

import sys
import time
import json
import os
import httpx

LITELLM_URL = os.environ.get("LITELLM_URL", "http://<LITELLM_HOST>:4000/v1/chat/completions")
API_KEY = os.environ.get("LITELLM_API_KEY", "")
if not API_KEY:
    print("ERROR: LITELLM_API_KEY not set")
    print("  → export LITELLM_API_KEY=sk-litellm-...")
    sys.exit(1)
RUNS = 20

MODELS = {
    "Opus 4.6":   "claude-opus-4-6",
    "Sonnet 4.6": "claude-sonnet-4-6",
}

client = httpx.Client(timeout=120.0)


def measure(model, prompt, max_tokens=256):
    body = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens,
        "stream": True,
        "stream_options": {"include_usage": True},
    }
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {API_KEY}",
    }

    t_start = time.perf_counter()
    first_token_time = None
    output_tokens = 0
    finish_reason = None

    with client.stream("POST", LITELLM_URL, json=body, headers=headers) as resp:
        if resp.status_code != 200:
            error_body = resp.read().decode()
            raise Exception(f"HTTP {resp.status_code}: {error_body[:200]}")

        for line in resp.iter_lines():
            if not line.startswith("data: "):
                continue
            data = line[6:]
            if data == "[DONE]":
                break
            try:
                chunk = json.loads(data)
            except json.JSONDecodeError:
                continue

            choices = chunk.get("choices", [])
            if not choices:
                continue
            delta = choices[0].get("delta", {})
            content = delta.get("content", "")
            if content and first_token_time is None:
                first_token_time = time.perf_counter()
            if content:
                output_tokens += 1

            fr = choices[0].get("finish_reason")
            if fr:
                finish_reason = fr

            usage = chunk.get("usage")
            if usage and "completion_tokens" in usage:
                output_tokens = usage["completion_tokens"]

    t_end = time.perf_counter()
    if first_token_time is None:
        return None

    ttft_ms = (first_token_time - t_start) * 1000
    e2e_ms = (t_end - t_start) * 1000
    gen_time = t_end - first_token_time
    throughput = output_tokens / gen_time if gen_time > 0 else 0

    return {
        "ttft_ms": round(ttft_ms, 1),
        "e2e_ms": round(e2e_ms, 1),
        "output_tokens": output_tokens,
        "throughput_tps": round(throughput, 1),
    }


def run_test(name, model, prompt, max_tokens=256, runs=RUNS):
    print(f"{'='*65}")
    print(f"  Model: {name} ({model}) via LiteLLM")
    print(f"  max_tokens: {max_tokens}, runs: {runs}")
    print(f"{'='*65}")

    # warmup
    print("  [warmup] ...", end="", flush=True)
    try:
        measure(model, "hi", max_tokens=10)
        print(" done")
    except Exception as e:
        print(f" FAILED: {e}")
        return

    results = []
    for i in range(runs):
        print(f"  [run {i+1}/{runs}] ", end="", flush=True)
        try:
            r = measure(model, prompt, max_tokens)
            if r:
                results.append(r)
                print(f"TTFT={r['ttft_ms']:>8.1f}ms  "
                      f"E2E={r['e2e_ms']:>8.1f}ms  "
                      f"tokens={r['output_tokens']:>4}  "
                      f"throughput={r['throughput_tps']:>6.1f} t/s")
            else:
                print("NO OUTPUT")
        except Exception as e:
            print(f"ERROR: {e}")

    if results:
        n = len(results)
        avg_ttft = sum(r['ttft_ms'] for r in results) / n
        avg_e2e = sum(r['e2e_ms'] for r in results) / n
        avg_tps = sum(r['throughput_tps'] for r in results) / n
        print(f"\n  >>> AVG  TTFT={avg_ttft:.1f}ms  E2E={avg_e2e:.1f}ms  throughput={avg_tps:.1f} t/s")


if __name__ == "__main__":
    prompt = "Write a detailed explanation of how TCP/IP networking works, including all 4 layers."

    for name, model in MODELS.items():
        print()
        run_test(name, model, prompt, max_tokens=256, runs=RUNS)

    client.close()
    print("\nDone.")
