#!/usr/bin/env python3
"""Probe each CodeArts AK/SK individually (bypassing the gateway's pool
rotation) to determine which account can actually chat, and which model
families each has access to."""
import hashlib
import hmac
import json
import sys
import time
import urllib.request
import urllib.error

BASE = "https://snap-access.cn-north-4.myhuaweicloud.com"


def sha256_hex(s: str) -> str:
    return hashlib.sha256(s.encode()).hexdigest()


def sign_chat(ak, sk, model, body, benefit=False):
    sdk_date = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    h = {
        "content-type": "application/json", "accept": "text/event-stream",
        "client_version": "Vscode_26.9.101", "agent-type": "ChatAgent",
        "x-language": "en-us", "plugin-name": "snap_vscode", "plugin-version": "26.9.101",
        "is_confidential": "false", "x-sdk-date": sdk_date,
    }
    if benefit:
        h["model-id"] = model
        h["model-name"] = model
        h["x-model-id"] = model
    names = sorted(h)
    header_lines = "\n".join(f"{k}:{h[k]}" for k in names)
    payload_hash = sha256_hex(body)
    canonical = "\n".join(["POST", "/api/v2/chat/completions/", "", header_lines, "",
                           ";".join(names), payload_hash])
    sts = "SDK-HMAC-SHA256\n" + sdk_date + "\n" + sha256_hex(canonical)
    sig = hmac.new(sk.encode(), sts.encode(), hashlib.sha256).hexdigest()
    h["Authorization"] = f"SDK-HMAC-SHA256 Access={ak},SignedHeaders={';'.join(names)},Signature={sig}"
    return h


def fetch_balance(ak, sk):
    """snap-manager statistics endpoint (region-API signing: signs host)."""
    url = BASE + "/snap-manager/v1/statistics/plugin"
    sdk_date = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    payload_hash = sha256_hex("")
    h = {
        "host": "snap-access.cn-north-4.myhuaweicloud.com",
        "x-sdk-date": sdk_date,
        "x-sdk-content-sha256": payload_hash,
    }
    names = sorted(h)
    header_lines = "\n".join(f"{k}:{h[k]}" for k in names)
    canonical = "\n".join(["GET", "/snap-manager/v1/statistics/plugin/", "", header_lines, "",
                           ";".join(names), payload_hash])
    sts = "SDK-HMAC-SHA256\n" + sdk_date + "\n" + sha256_hex(canonical)
    sig = hmac.new(sk.encode(), sts.encode(), hashlib.sha256).hexdigest()
    h["Authorization"] = f"SDK-HMAC-SHA256 Access={ak},SignedHeaders={';'.join(names)},Signature={sig}"
    req = urllib.request.Request(url, headers=h)
    req.add_header("Agent-Type", "PromptCenter")
    req.add_header("X-Language", "zh-cn")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            j = json.loads(r.read())
        pkg = j.get("package", {})
        credits = 0.0
        for m in j.get("metrics", []):
            if m.get("name") == "usageTotalPackageCredit":
                credits = m.get("package_credit_amount", 0)
        return f"套餐={pkg.get('spec_display_name') or pkg.get('spec_name','?')} 积分={credits} 积分账户={pkg.get('is_credit_package')}"
    except urllib.error.HTTPError as e:
        return f"HTTP {e.code}: {e.read().decode(errors='replace')[:120]}"
    except Exception as e:
        return str(e)[:120]


def chat(ak, sk, model, benefit, label):
    body = json.dumps({"model": model, "stream": False, "max_tokens": 32,
                       "messages": [{"role": "user", "content": "只回复：成功"}]})
    if benefit:
        b = json.loads(body)
        b["maas_type"] = "benefit"
        body = json.dumps(b)
    hdrs = sign_chat(ak, sk, model, body, benefit)
    req = urllib.request.Request(BASE + "/api/v2/chat/completions", data=body.encode(),
                                 headers=hdrs, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            raw = r.read().decode(errors="replace")
            # SSE or JSON
            if raw.startswith("{"):
                j = json.loads(raw)
                c = (j.get("choices") or [{}])[0].get("message", {}).get("content", "")
                return f"OK 200 content={c[:20]!r}"
            for line in raw.splitlines():
                if line.startswith("data:") and "[DONE]" not in line:
                    try:
                        chunk = json.loads(line[5:])
                        c = (chunk.get("choices") or [{}])[0].get("delta", {}).get("content")
                        if c:
                            return f"OK 200 (SSE) content={c[:20]!r}"
                    except Exception:
                        pass
            return f"200 空流 :: {raw[:100]}"
    except urllib.error.HTTPError as e:
        d = e.read().decode(errors="replace")[:150]
        return f"HTTP {e.code}: {d}"
    except Exception as e:
        return f"{type(e).__name__}: {e}"


def main():
    d = json.load(open(r"D:\buddyhub\data\ext-accounts.json", encoding="utf-8"))
    accs = [a for a in d["accounts"] if a.get("provider") == "codearts"]

    cases = [
        ("openpangu-2.0-pro", False, "agent/旗舰"),
        ("openpangu-2.0-flash", False, "agent/快"),
        ("GLM-5.2", False, "agent/GLM"),
        ("deepseek-v4-flash-0731", True, "benefit/免费"),
        ("glm-5.3-flash", True, "benefit/免费"),
    ]
    for a in accs:
        c = a["cred"]
        if isinstance(c, str):
            c = json.loads(c)
        ak, sk = c["access_key_id"], c["secret_access_key"]
        print(f"=== {a['id']} (AK={ak[:8]}…) ===")
        print(f"  余额: {fetch_balance(ak, sk)}")
        for model, benefit, tag in cases:
            print(f"  [{tag:11}] {model:26} -> {chat(ak, sk, model, benefit, tag)}")
        print()


if __name__ == "__main__":
    main()
