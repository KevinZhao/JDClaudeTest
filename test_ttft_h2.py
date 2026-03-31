#!/usr/bin/env python3
"""Bedrock TTFT Test — httpx HTTP/2 + Bearer Token
使用 httpx + h2 实现 HTTP/2，Bearer Token 认证。
"""

import sys
import time
import json
import os
import base64

import httpx

# ── 环境检查 ──────────────────────────────────────────────
def check_env():
    print(f"httpx:    {httpx.__version__}")
    try:
        import h2
        print(f"h2:       {h2.__version__} ✅ (HTTP/2)")
    except ImportError:
        print("h2:       NOT INSTALLED ❌")
        print("  → pip install httpx[http2]")
        sys.exit(1)
    print()

check_env()

# ── 配置 ────────────────────────────────────────────────────
NLB_HOST = "bedrock-nlb-8b69cce82f32c532.elb.ap-northeast-1.amazonaws.com"
BEDROCK_HOST = "bedrock-runtime.ap-northeast-1.amazonaws.com"
RUNS = 3

MODELS = {
    "Opus 4.6":   "global.anthropic.claude-opus-4-6-v1",
    "Sonnet 4.6": "global.anthropic.claude-sonnet-4-6",
}

BEARER_TOKEN = os.environ.get("AWS_BEARER_TOKEN_BEDROCK", "")
if not BEARER_TOKEN:
    print("ERROR: AWS_BEARER_TOKEN_BEDROCK not set")
    print("  → export AWS_BEARER_TOKEN_BEDROCK=ABSK...")
    sys.exit(1)


# ── NLB DNS 重写 ──────────────────────────────────────────
import socket
_orig_getaddrinfo = socket.getaddrinfo
_nlb_ips = None

def _resolve_nlb():
    global _nlb_ips
    if _nlb_ips is None:
        _nlb_ips = _orig_getaddrinfo(NLB_HOST, 443, socket.AF_INET, socket.SOCK_STREAM)
        print(f"NLB resolved: {[a[4][0] for a in _nlb_ips]}")
    return _nlb_ips

def _patched_getaddrinfo(host, port, *args, **kwargs):
    if host == BEDROCK_HOST:
        return _resolve_nlb()
    return _orig_getaddrinfo(host, port, *args, **kwargs)

socket.getaddrinfo = _patched_getaddrinfo


# ── httpx HTTP/2 client ─────────────────────────────────────
http2_client = httpx.Client(http2=True, timeout=120.0, verify=True)

# 验证 HTTP/2
def verify_http2():
    url = f"https://{BEDROCK_HOST}/model/{MODELS['Sonnet 4.6']}/invoke"
    body = json.dumps({
        "anthropic_version": "bedrock-2023-05-31",
        "max_tokens": 1,
        "messages": [{"role": "user", "content": "hi"}],
    }).encode()
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {BEARER_TOKEN}",
    }
    resp = http2_client.post(url, content=body, headers=headers)
    print(f"HTTP version: {resp.http_version}")
    print(f"Status: {resp.status_code}")
    if resp.http_version == "HTTP/2":
        print("HTTP/2 confirmed ✅")
    else:
        print("WARNING: NOT HTTP/2 ❌")
    print()

verify_http2()


# ── 测量 ────────────────────────────────────────────────────
def measure(model_id, prompt, max_tokens=256):
    from botocore.eventstream import EventStreamBuffer

    url = f"https://{BEDROCK_HOST}/model/{model_id}/invoke-with-response-stream"
    body = json.dumps({
        "anthropic_version": "bedrock-2023-05-31",
        "max_tokens": max_tokens,
        "messages": [{"role": "user", "content": prompt}],
    }).encode()
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {BEARER_TOKEN}",
    }

    t_start = time.perf_counter()
    first_token_time = None
    output_tokens = 0

    event_stream = EventStreamBuffer()

    with http2_client.stream("POST", url, content=body, headers=headers) as resp:
        if resp.status_code != 200:
            error_body = resp.read().decode()
            raise Exception(f"HTTP {resp.status_code}: {error_body[:200]}")

        for raw_bytes in resp.iter_bytes():
            event_stream.add_data(raw_bytes)
            for event in event_stream:
                event_type = ""
                for h_name, h_val in event.headers.items():
                    if h_name == ":event-type":
                        event_type = h_val
                        break

                if event_type == "chunk" and event.payload:
                    payload = json.loads(event.payload)
                    chunk_bytes = payload.get("bytes", "")
                    if chunk_bytes:
                        chunk_data = json.loads(base64.b64decode(chunk_bytes))
                        chunk_type = chunk_data.get("type", "")

                        if chunk_type == "content_block_delta":
                            if first_token_time is None:
                                first_token_time = time.perf_counter()
                            output_tokens += 1
                        elif chunk_type == "message_delta":
                            usage = chunk_data.get("usage", {})
                            output_tokens = usage.get("output_tokens", output_tokens)

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


def run_test(name, model_id, prompt, max_tokens=256, runs=RUNS):
    print(f"{'='*65}")
    print(f"  Model: {name} ({model_id})")
    print(f"  max_tokens: {max_tokens}, runs: {runs}, protocol: HTTP/2")
    print(f"{'='*65}")

    # warmup
    print("  [warmup] ...", end="", flush=True)
    try:
        measure(model_id, "hi", max_tokens=10)
        print(" done")
    except Exception as e:
        print(f" FAILED: {e}")
        return

    results = []
    for i in range(runs):
        print(f"  [run {i+1}/{runs}] ", end="", flush=True)
        try:
            r = measure(model_id, prompt, max_tokens)
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

    for name, model_id in MODELS.items():
        print()
        run_test(name, model_id, prompt, max_tokens=256, runs=RUNS)

    http2_client.close()
    socket.getaddrinfo = _orig_getaddrinfo
    print("\nDone.")
