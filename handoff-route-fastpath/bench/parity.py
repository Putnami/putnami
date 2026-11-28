#!/usr/bin/env python3
"""Send the parity request set to a subject and print a normalized answer per request.

Usage: parity.py <port>            print the normalized answers as JSON lines
       parity.py <port> <port>...  compare subjects, exit 1 on any difference
"""
import http.client, json, re, sys

ID = "00000000-0000-4000-8000-000000000001"
MISSING = "00000000-0000-4000-8000-0000000fffff"
BIG = json.dumps({"title": "x", "description": "y" * (1024 * 1024 + 10)})
J = {"Content-Type": "application/json"}
REQS = [
    ("GET", "/health", None, {}),
    ("HEAD", "/health", None, {}),
    ("OPTIONS", "/health", None, {}),
    ("POST", "/health", "{}", J),
    ("GET", f"/tasks/{ID}", None, {}),
    ("GET", f"/tasks/{MISSING}", None, {}),
    ("GET", "/tasks/not-a-uuid", None, {}),
    ("HEAD", f"/tasks/{ID}", None, {}),
    ("OPTIONS", f"/tasks/{ID}", None, {}),
    ("DELETE", f"/tasks/{ID}", None, {}),
    ("GET", "/tasks?limit=3", None, {}),
    ("OPTIONS", "/tasks", None, {}),
    ("POST", "/tasks", json.dumps({"title": "parity", "priority": 3}), J),
    ("POST", "/tasks", json.dumps({"title": "a" * 201}), J),
    ("POST", "/tasks", json.dumps({"title": "\U0001F600" * 150}), J),  # 150 code points, 300 UTF-16 units
    ("POST", "/tasks", json.dumps({"title": "\u00e9" * 200}), J),       # 200 units: accepted
    ("POST", "/tasks", json.dumps({"description": "no title"}), J),
    ("POST", "/tasks", "{not json", J),
    ("POST", "/tasks", BIG, J),
    ("POST", "/tasks", json.dumps({"title": "x"}), {**J, "Origin": "http://evil.example"}),
    ("POST", "/tasks", json.dumps({"title": "x"}), {**J, "Sec-Fetch-Site": "cross-site", "Origin": "http://evil.example"}),
    ("GET", "/nope", None, {}),
]
KEEP = ["allow", "content-type", "x-content-type-options", "content-security-policy", "x-frame-options",
        "referrer-policy", "permissions-policy", "x-xss-protection"]
UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
TS = re.compile(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z")


def norm_ct(v):
    return v.split(";")[0].strip().lower() if v else v


def probe(port):
    out = []
    for method, path, body, headers in REQS:
        c = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
        try:
            c.request(method, path, body=body, headers=headers)
            r = c.getresponse()
            data = r.read()
            h = {k.lower(): v for k, v in r.getheaders()}
            st = r.status
        except Exception as e:  # a server that closes on an oversized body
            out.append({"req": f"{method} {path[:40]}", "status": f"conn:{type(e).__name__}"})
            continue
        finally:
            c.close()
        rec = {"req": f"{method} {path[:40]}", "status": st}
        for k in KEEP:
            if k in h:
                v = h[k]
                if k == "content-type":
                    v = norm_ct(v)
                if k == "allow":
                    v = ",".join(sorted(x.strip() for x in v.split(",")))
                rec[k] = v
        if st < 300 and data and norm_ct(h.get("content-type")) == "application/json":
            rec["body"] = TS.sub("<ts>", UUID.sub("<uuid>", data.decode()))
        out.append(rec)
    return out


def main():
    ports = [int(p) for p in sys.argv[1:]]
    if len(ports) == 1:
        for r in probe(ports[0]):
            print(json.dumps(r))
        return 0
    results = [probe(p) for p in ports]
    diffs = 0
    for i, base in enumerate(results[0]):
        for p, res in zip(ports[1:], results[1:]):
            other = res[i]
            if other != base:
                diffs += 1
                print(f"DIFF {base['req']}\n  {ports[0]}: {json.dumps(base)}\n  {p}: {json.dumps(other)}")
    print(f"{len(REQS)} requests, {diffs} differences")
    return 1 if diffs else 0


sys.exit(main())
