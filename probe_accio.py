#!/usr/bin/env python3
"""Probe the accio bridge with the *exact* request the gateway would build,
so we can see the real ADK error frame behind the 502 'invalid params'.

Builds the ADK envelope the same way accio.BuildUpstreamBody does, POSTs it
to phoenix-gw, and dumps the raw SSE/JSON. No gateway needed — talks to the
upstream directly using the pool's stored accio access token.
"""
import hashlib
import json
import urllib.request
import urllib.error
import sys

CFG = r"D:\buddyhub\config.json"
GATEWAY = "https://phoenix-gw.alibaba.com"
ADK_PATH = "/api/adk/llm"
TENANT = "accio-agent"
IAI_TAG = "phoenix-desktop"
APP_KEY = "35298846"
APP_VERSION = "0.32.6"
CLIENT_ID = "accio-work"


def gateway_key():
    return json.load(open(CFG, encoding="utf-8")).get("api_key", "")


def accio_token():
    path = r"D:\buddyhub\data\ext-accounts.json"
    data = json.load(open(path, encoding="utf-8"))
    accounts = data.get("accounts", data) if isinstance(data, dict) else data
    for a in accounts:
        if a.get("provider") == "accio":
            cred = a.get("cred") or {}
            if isinstance(cred, str):
                cred = json.loads(cred)
            return cred.get("access_token", ""), cred.get("device_id", ""), cred.get("region", "")
    return "", "", ""


def adk_chat(model, token, device, reqid, gateway):
    """POST one ADK chat request, return (http_status, body_snippet)."""
    body = {
        "model": model, "tenant": TENANT, "iai_tag": IAI_TAG, "empid": "",
        "request_id": reqid, "token": token,
        "contents": [{"role": "user", "parts": [{"text": "只回复：成功"}]}],
        "tool_config": json.dumps({"functionCallingConfig": {"streamFunctionCallArguments": True}}),
        "properties": {"normalized_response": "true"},
    }
    if device:
        body["device_id"] = device
    raw_body = json.dumps(body).encode()
    sg = hashlib.md5(reqid.encode()).hexdigest()
    url = f"{gateway}{ADK_PATH}/generateContent?sg_k={sg}"
    req = urllib.request.Request(url, data=raw_body, method="POST", headers={
        "Content-Type": "application/json", "Accept": "text/event-stream",
        "Accept-Language": "en", "x-app-key": APP_KEY, "x-app-version": APP_VERSION,
        "x-client-id": CLIENT_ID, "Authorization": "Bearer " + token,
    })
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return 200, r.read().decode(errors="replace")[:1200]
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")[:400]



def GATEWAY_BASE_EXT():
    base = "http://127.0.0.1:7863"
    return base + "/panel/api/ext/accounts"


def pick_from_catalog(token, device, gateway):
    """Pull model ids from POST /api/llm/config/v2; fall back to None if it 403s."""
    try:
        req = urllib.request.Request(
            gateway + "/api/llm/config/v2", data=b"{}", method="POST",
            headers={"Content-Type": "application/json", "Accept": "application/json",
                     "Accept-Language": "en", "x-app-key": APP_KEY,
                     "x-app-version": APP_VERSION, "x-client-id": CLIENT_ID,
                     "Authorization": "Bearer " + token})
        with urllib.request.urlopen(req, timeout=30) as r:
            j = json.loads(r.read().decode(errors="replace"))
        models = (j.get("data") or {}).get("models") or j.get("models") or []
        ids = [m.get("id") or m.get("model_id") or m.get("name") for m in models
               if isinstance(m, dict)]
        ids = [i for i in ids if i]
        if ids:
            print(f"(catalog models: {ids[:12]})")
            return ids[0]
    except Exception as e:
        print(f"(catalog fetch failed: {e})")
    return None


def main():
    token, device, region = accio_token()
    model = sys.argv[1] if len(sys.argv) > 1 and sys.argv[1] != "--models" else "gemini-3-flash-preview"
    print(f"region={region or '(unset)'} device={device[:12] if device else '(none)'} token_present={bool(token)}")
    if not token:
        print("no accio access token in pool; aborting")
        return

    # First: list models via POST (the bridge's method). If it yields usable
    # model ids, retry the chat with the first one — the "invalid params" is
    # most likely just a wrong model name, not a credential problem.
    model = sys.argv[1] if len(sys.argv) > 1 and sys.argv[1] not in ("--models", "--models-get") else ""
    if not model:
        model = pick_from_catalog(token, device, GATEWAY)
        if not model:
            model = "gemini-3-flash-preview"
        print(f"(picked model from catalog or fallback: {model})")
    print(f"region={region or '(unset)'} device={device[:12] if device else '(none)'} token_present={bool(token)}")
    if not token:
        print("no accio access token in pool; aborting")
        return

    reqid = "accprobe00000000000001"
    status, body_snip = adk_chat(model, token, device, reqid, GATEWAY)
    print(f"[{model}] HTTP {status}")
    print(body_snip)


def list_models(method="POST"):
    """Probe /api/llm/config/v2 with POST (what the bridge uses) or GET, to see
    which method the upstream accepts and what models it lists."""
    token, device, region = accio_token()
    if not token:
        print("no token"); return
    body = b"{}" if method == "POST" else None
    req = urllib.request.Request(
        GATEWAY + "/api/llm/config/v2",
        data=body, method=method,
        headers={"Content-Type": "application/json", "Accept": "application/json",
                 "Accept-Language": "en", "x-app-key": APP_KEY,
                 "x-app-version": APP_VERSION, "x-client-id": CLIENT_ID,
                 "Authorization": "Bearer " + token})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read().decode(errors="replace")
            print(f"models [{method}] HTTP {r.status} len={len(raw)}")
            print(raw[:2000])
    except urllib.error.HTTPError as e:
        print(f"models [{method}] HTTP {e.code}: {e.read().decode(errors='replace')[:300]}")


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--models":
        list_models("POST")
    elif len(sys.argv) > 1 and sys.argv[1] == "--models-get":
        list_models("GET")
    else:
        main()
