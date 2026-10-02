"""Probe codearts models + agent-center catalog via the running gateway.

Reads the api_key from D:\\buddyhub\\config.json internally (no credential on
the CLI). For each candidate model name, sends a tiny chat request through
/v1/chat/completions and reports whether it reaches the upstream (200 / a real
answer) or errors. Also queries /panel/api/models to see which codearts: entries
the catalog exposed.
"""
import json
import sys
import urllib.request
import urllib.error

CFG = r"D:\buddyhub\config.json"
BASE = "http://127.0.0.1:7863"


def key():
    return json.load(open(CFG, encoding="utf-8")).get("api_key", "")


def models():
    k = key()
    req = urllib.request.Request(BASE + "/panel/api/models",
                                 headers={"Authorization": "Bearer " + k})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            j = json.loads(r.read().decode(errors="replace"))
    except urllib.error.HTTPError as e:
        return f"panel/models HTTP {e.code}: {e.read().decode(errors='replace')[:200]}"
    # Find the platform listing; the panel returns models grouped by provider.
    flat = json.dumps(j)
    return j


def chat(model, label):
    k = key()
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": "只回复：成功"}],
        "stream": False,
        "max_tokens": 32,
    }).encode()
    req = urllib.request.Request(BASE + "/v1/chat/completions", data=body,
                                method="POST",
                                headers={"Content-Type": "application/json",
                                         "Authorization": "Bearer " + k})
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            raw = r.read().decode(errors="replace")
            j = json.loads(raw)
            ok = bool((j.get("choices") or [{}])[0].get("content"))
            return f"[{label}] {model} -> 200 ok={ok} :: {raw[:140]}"
    except urllib.error.HTTPError as e:
        return f"[{label}] {model} -> HTTP {e.code} :: {e.read().decode(errors='replace')[:240]}"
    except Exception as e:
        return f"[{label}] {model} -> {type(e).__name__}: {e}"


def main():
    cats = ["codearts:openpangu-2.0-pro", "codearts:GLM-5.2",
            "codearts:deepseek-v4-flash-0731", "codearts:deepseek-v4-pro-0813",
            "codearts:glm-5.3-flash"]
    for m in cats:
        print(chat(m, "codearts"), flush=True)
    # dump what /panel/api/models exposes under codearts
    j = models()
    print("\n== panel /models shape ==")
    s = json.dumps(j, ensure_ascii=False)
    import re
    # show any substring near "codearts"
    for mobj in re.finditer(r"codearts[^\"]{0,60}", s):
        pass
    print(s[:600])


if __name__ == "__main__":
    main()
