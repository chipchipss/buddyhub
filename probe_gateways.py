#!/usr/bin/env python3
"""Probe the running buddyhub gateway's external-platform health.

Reads the api_key from D:\\buddyhub\\config.json (no credential on the
command line), then hits each external bridge's chat endpoint with a tiny
request and reports the upstream error per platform. Read-only: it does not
modify pool state — only the in-process backoff timers, which are harmless.

Usage:
    python3 probe_gateways.py [--base http://127.0.0.1:7863]
"""
import argparse
import json
import sys
import urllib.request
import urllib.error

# (model, prefix label) — one representative chat request per external bridge.
# The model name after the prefix must be a real upstream model; where unsure
# we still learn the *auth/permission* class of error, which is what matters
# for "can this platform be called at all".
PROBES = [
    ("qoder:Qwen-3-Coder", "qoder"),
    ("raccoon:AutoGLM-4-30B", "raccoon"),
    ("cline:cline-free/deepseek-v4.1-flash", "cline"),
    ("autoclaw:autoglm", "autoclaw"),
    ("qclaw:gpt-4o", "qclaw"),
    ("trae:deepseek-v4", "trae"),
    ("accio:gemini-3-flash-preview", "accio"),
    ("traework:deepseek-v4", "traework"),
    ("marvis:deepseek-v4", "marvis"),
    ("ima:deepseek-v4", "ima"),
    ("codearts:deepseek-v4", "codearts"),
    ("copilot:deepseek-v4", "copilot"),
]


def read_key(base):
    cfg = json.load(open(base.replace("http://", "D:/") and "D:/buddyhub/config.json"
                         or "D:/buddyhub/config.json", encoding="utf-8"))
    return cfg.get("api_key", "")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:7863")
    args = ap.parse_args()
    key = read_key(args.base)
    if not key:
        sys.exit("no api_key in config.json")
    base = args.base.rstrip("/")

    for model, label in PROBES:
        url = base + "/v1/chat/completions"
        body = json.dumps({
            "model": model,
            "messages": [{"role": "user", "content": "ping"}],
            "stream": False,
            "max_tokens": 8,
        }).encode()
        req = urllib.request.Request(url, data=body, method="POST",
                                     headers={"Content-Type": "application/json",
                                              "Authorization": "Bearer " + key})
        try:
            with urllib.request.urlopen(req, timeout=45) as r:
                raw = r.read().decode(errors="replace")
                j = json.loads(raw or "{}")
                ok = ("choices" in j) and (j.get("choices") or [{}])[0].get("content")
                msg = "OK" if ok else "200-but-empty"
                print(f"[{label:9}] {msg}: {raw[:120]}")
        except urllib.error.HTTPError as e:
            detail = e.read().decode(errors="replace")[:200]
            print(f"[{label:9}] HTTP {e.code}: {detail}")
        except Exception as e:
            print(f"[{label:9}] {type(e).__name__}: {e}")


if __name__ == "__main__":
    main()
