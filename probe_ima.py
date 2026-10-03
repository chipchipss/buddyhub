#!/usr/bin/env python3
"""Direct ima.qq.com protocol probe — calls init_session + qa with the stored
cookie and dumps the REAL upstream responses, so we can see whether it's the
init_session body, the bkn, or the cookie that's off."""
import json
import urllib.request
import urllib.error

IMA = "https://ima.qq.com"


def bkn(token: str) -> str:
    h = 5381
    for ch in token:
        h += (h << 5) + ord(ch)
    return str(h & 0x7FFFFFFF)


def cookie_field(cookie, key):
    for part in cookie.split(";"):
        part = part.strip()
        if "=" in part and part.split("=", 1)[0] == key:
            return part.split("=", 1)[1]
    return ""


def headers(cookie):
    token = cookie_field(cookie, "IMA-TOKEN")
    return {
        "from_browser_ima": "1",
        "x-ima-cookie": cookie,
        "x-ima-bkn": bkn(token),
        "referer": IMA,
        "origin": IMA,
        "User-Agent": "okhttp/4.12.0",
        "Content-Type": "application/json; charset=utf-8",
        "Accept-Encoding": "gzip",
    }


def post(path, body, cookie):
    data = json.dumps(body).encode()
    req = urllib.request.Request(IMA + path, data=data, method="POST",
                                 headers=headers(cookie))
    req.add_header("Content-Length", str(len(data)))
    try:
        import gzip
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read()
            if r.headers.get("Content-Encoding") == "gzip":
                raw = gzip.decompress(raw)
            return r.status, raw.decode(errors="replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")
    except Exception as e:
        return -1, repr(e)


def main():
    d = json.load(open(r"D:\buddyhub\data\ext-accounts.json", encoding="utf-8"))
    accs = d.get("accounts", d)
    ima = [a for a in accs if a.get("provider") == "ima"]
    cred = ima[0]["cred"]
    if isinstance(cred, str):
        cred = json.loads(cred)
    cookie = cred["cookie"]

    # 1) plain init_session — exactly what our Go code sends
    status, raw = post("/cgi-bin/session_logic/init_session",
                       {"env_info": {"interact_type": 2, "robot_type": 10000},
                        "name": "新对话", "msgs_limit": 20}, cookie)
    print(f"[init plain] HTTP {status}: {raw[:400]}")

    # 2) richer init_session — the tencent-ima-copilot-mcp form
    status2, raw2 = post("/cgi-bin/session_logic/init_session",
                         {"env_info": {"robot_type": 10000, "interact_type": 0},
                          "related_url": "7305806844290061",
                          "scene_type": 0,
                          "msgs_limit": 10,
                          "forbid_auto_add_to_history_list": False,
                          "knowledge_base_info_with_folder": {
                              "knowledge_base_id": "7305806844290061",
                              "folder_ids": []}}, cookie)
    print(f"[init rich]  HTTP {status2}: {raw2[:400]}")
    try:
        j = json.loads(raw2)
        if j.get("code") == 0:
            sid = j.get("data", {}).get("session_id") or j.get("session_id")
            print(f"  session_id: {sid}")
            # 3) a quick QA against it
            st3, raw3 = post("/cgi-bin/assistant/qa",
                             {"session_id": sid, "robot_type": 10000,
                              "question": "只回复：成功", "question_type": 2,
                              "command_info": {"question_info": {}},
                              "client_id": "probe-0001",
                              "model_info": {"model_type": 0, "model_id": "official_0"}},
                             cookie)
            print(f"[qa]        HTTP {st3}: {raw3[:400]}")
        else:
            print(f"  code={j.get('code')} msg={j.get('msg')}")
    except Exception as e:
        print("  parse err:", e)


if __name__ == "__main__":
    main()
