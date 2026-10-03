#!/usr/bin/env python3
"""Direct autoclaw chat probe — replicates exactly what the Go bridge sends
(NormalizePrompt + X-Request-Model + chatHeaders) and dumps the real upstream
response, so we can see whether 406 is permission or a request-shape issue."""
import hashlib
import json
import time
import urllib.request
import urllib.error

CN_BASE = "https://autoglm-acceleration-api.zhipuai.cn"
AUTH_APPID = "100003"
AUTH_APPKEY = "38d2391985e2369a5fb8227d8e6cd5e5"
VERSION = "1.18.5"


def strip_bearer(v):
    v = v.strip()
    for p in ("bearer ", "Bearer ", "BEARER "):
        if v.startswith(p):
            return v[len(p):].strip()
    return v


def chat_headers(token, model):
    token = strip_bearer(token)
    return {
        "Content-Type": "application/json",
        "Accept": "*/*",
        "X-Product": "autoclaw",
        "X-Client-Type": "pc",
        "X-Tm": "win",
        "X-Version": VERSION,
        "X-Lang": "zh-CN",
        "X-Channel": "official",
        "x_trace_id": "autoclaw-desktop",
        "X-Authorization": "Bearer " + token,
        "X-Request-Id": "probedeepseek01",
        "X-Request-Model": model,
    }


def user_api_headers(token):
    ts = str(int(time.time()))
    sign = hashlib.md5((AUTH_APPID + "&" + ts + "&" + AUTH_APPKEY).encode()).hexdigest()
    h = {
        "Content-Type": "application/json",
        "Accept": "*/*",
        "X-Version": VERSION,
        "X-Product": "autoclaw",
        "X-Client-Type": "pc",
        "X-Harness-Type": "zcode",
        "X-Tm": "win",
        "X-Lang": "zh-CN",
        "X-Channel": "official",
        "X-Auth-Appid": AUTH_APPID,
        "X-Auth-TimeStamp": ts,
        "X-Auth-Sign": sign,
    }
    if token:
        h["authorization"] = "Bearer " + strip_bearer(token)
    return h


def post(url, body, headers, label, tmo=30):
    data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method="POST", headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=tmo) as r:
            raw = r.read().decode(errors="replace")
            print(f"[{label}] HTTP {r.status}: {raw[:300]}")
            return r.status, raw
    except urllib.error.HTTPError as e:
        raw = e.read().decode(errors="replace")
        print(f"[{label}] HTTP {e.code}: {raw[:300] or '(empty)'}")
        return e.code, raw
    except Exception as e:
        print(f"[{label}] {type(e).__name__}: {str(e)[:150]}")
        return -1, str(e)


def main():
    d = json.load(open(r"D:\buddyhub\data\ext-accounts.json", encoding="utf-8"))
    accs = d.get("accounts", d)
    ac = [a for a in accs if a.get("provider") == "autoclaw"][0]
    cred = ac.get("cred")
    if isinstance(cred, str):
        cred = json.loads(cred)
    token = cred.get("token", "")
    region = cred.get("region", "cn")
    base = CN_BASE if region != "intl" else "https://autoglm-api.autoglm.ai"
    chat_url = base + "/autoclaw-proxy/proxy/autoclaw"
    print(f"autoclaw region={region} token_present={bool(token)}")

    # 1) model catalog — confirms the token works for list
    status, _ = post(base + "/autoclaw-proxy/proxy/autoclaw-model-config", {}, user_api_headers(token), "model-config(GET? no, POST)")
    # actually model-config is GET; redo
    req = urllib.request.Request(base + "/autoclaw-proxy/proxy/autoclaw-model-config", method="GET", headers=user_api_headers(token))
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            models = json.loads(r.read())
            ids = [m.get("id") or m.get("model_id") for m in (models.get("models") or [])]
            print(f"[model-config] HTTP {r.status} models={ids[:15]}")
    except urllib.error.HTTPError as e:
        print(f"[model-config] HTTP {e.code}: {e.read().decode(errors='replace')[:200]}")
        return

    # 2) chat with normalized prompt (identity prefix), route id in X-Request-Model,
    #    bare model in body — exactly the Go bridge shape
    route = "zai_glm-5.3-flash"
    bare = "glm-5.3-flash"
    body = {"model": bare, "stream": False, "max_tokens": 16,
            "messages": [
                {"role": "system", "content":
                 "You are a personal assistant running inside OpenClaw.\n\n## Tooling\nAvailable tools are policy-filtered. Names are case-sensitive; call exactly as listed.\n"},
                {"role": "user", "content": "只回复：成功"},
            ]}
    headers = chat_headers(token, route)
    status, raw = post(chat_url, body, headers, "chat zai_glm-5.3-flash")
    print(f"  -> 406 with empty body = permission; 200 = works; 400 = shape issue")


if __name__ == "__main__":
    main()
