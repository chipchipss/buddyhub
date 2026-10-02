#!/usr/bin/env python3
"""Import a Huawei Cloud CodeArts AK/SK (or STS credential) into the
buddyhub panel's external account store.

Uses the same endpoint the panel UI uses:
    POST /panel/api/ext/accounts  { provider: "codearts", id, cred }

The panel shares the gateway's Bearer api_key (empty key = no auth).

Usage:
    python3 import_codearts.py <accessKeyCSV> [--host http://127.0.0.1:7863]
                               [--id CODEARTS-01] [--api-key KEY]

CSV shape (as exported from CodeArts / 码道 IDE):
    "Access key ID","Secret access key"
    "HPUA...","DhgL..."

STS credentials (temporary, expire in hours — not recommended for the pool)
can be passed via --security-token and/or --expires-at / --user-id /
--user-name when the CSV lacks those columns.

The account id is reused idempotently: re-running with the same --id
overwrites the credential in place (panel Add is upsert on provider+id).
"""
import argparse
import csv
import io
import json
import sys
import urllib.error
import urllib.request


def parse_csv(path):
    with open(path, "r", encoding="utf-8-sig") as f:
        rows = list(csv.reader(f))
    # Header row: "Access key ID","Secret access key"
    data = [r for r in rows if r and not r[0].lower().startswith("access key")]
    if not data:
        sys.exit("no credential rows in CSV")
    row = [c.strip().strip('"') for c in data[0]]
    ak, sk = row[0], row[1]
    cred = {
        "access_key_id": ak,
        "secret_access_key": sk,
    }
    # Optional extra columns for STS / audit metadata.
    extras = {
        2: "security_token",
        3: "expires_at",
        4: "user_id",
        5: "user_name",
    }
    for i, key in extras.items():
        if i < len(row) and row[i]:
            cred[key] = row[i]
    return cred


def read_key(base):
    """No --api-key given: fall back to the gateway config next to the panel."""
    path = "D:/buddyhub/config.json"
    try:
        return json.load(open(path, encoding="utf-8")).get("api_key", "")
    except Exception:
        return ""


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("csv", help="path to the accessKeys CSV")
    ap.add_argument("--host", default="http://127.0.0.1:7863",
                    help="gateway/panel base URL (default http://127.0.0.1:7863)")
    ap.add_argument("--id", default="CODEARTS-01",
                    help="account id in the pool (upsert key, default CODEARTS-01)")
    ap.add_argument("--label", default="", help="display label (default: id)")
    ap.add_argument("--api-key", default="",
                    help="panel Bearer api_key; empty = read from D:/buddyhub/config.json")
    ap.add_argument("--checkin", action="store_true",
                    help="also run today's CodeArts daily check-in after import")
    args = ap.parse_args()

    cred = parse_csv(args.csv)
    payload = {
        "provider": "codearts",
        "id": args.id,
        "label": args.label or args.id,
        "cred": cred,
    }

    base = args.host.rstrip("/")
    url = base + "/panel/api/ext/accounts"
    key = args.api_key or read_key(base)
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = "Bearer " + key

    req = urllib.request.Request(
        url, data=json.dumps(payload).encode(), headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            body = json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        detail = e.read().decode(errors="replace")[:500]
        sys.exit(f"HTTP {e.code} from {url}: {detail}")

    if not body.get("ok"):
        sys.exit(f"panel rejected import: {json.dumps(body, ensure_ascii=False)}")
    print(f"imported codearts/{args.id}  (ak={cred['access_key_id'][:8]}...)")

    if args.checkin:
        cu = base + f"/panel/api/ext/accounts/codearts/{args.id}/checkin"
        creq = urllib.request.Request(
            cu, data=b"{}", headers=headers, method="POST")
        try:
            with urllib.request.urlopen(creq, timeout=60) as r:
                cb = json.loads(r.read().decode() or "{}")
        except urllib.error.HTTPError as e:
            cb = {"ok": False, "error": e.read().decode(errors="replace")[:300]}
        res = cb.get("result", cb)
        print(f"checkin: {res.get('kind')} {res.get('message', res.get('error', ''))}")
        if res.get("kind") == "failed":
            sys.exit(1)


if __name__ == "__main__":
    main()
