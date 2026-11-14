"""universal-ai-gateway 实测脚本模板（live-test 技能）。复制到临时目录后修改 ACCOUNTS 与 main()。

只输出 PASS / FAIL、状态码与计数；最后检查输出与网关日志中没有任何敏感值。
用法：python livetest.py（与 gateway.exe、gw.boot.log 放在同一目录）
"""
import csv, json, os, re, subprocess, time, urllib.error, urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
DATA, ADMIN = "http://127.0.0.1:18496", "http://127.0.0.1:18497"
EXE = os.path.join(HERE, "gateway.exe" if os.name == "nt" else "gateway")
ACCOUNTS = r"<账号文件路径>"          # 只按路径使用，不打印内容
SENSITIVE_FIELDS = ("api_key", "password", "email", "phone", "username", "token")

results, printed = [], []


def load_sensitive(path):
    """收集账号文件中的敏感值（只用于泄露检查，从不打印）。"""
    vals = set()
    rows = csv.DictReader(open(path, encoding="utf-8-sig")) if path.lower().endswith(".csv") else \
        (json.loads(l) for l in open(path, encoding="utf-8") if l.strip())
    for r in rows:
        vals.update(v for k, v in r.items() if k in SENSITIVE_FIELDS and isinstance(v, str) and len(v) >= 6)
    return vals


def admin_token():
    for _ in range(30):
        m = re.search(r"BOOTSTRAP ADMIN TOKEN \(shown once\): (\S+)", open(os.path.join(HERE, "gw.boot.log"), encoding="utf-8").read())
        if m:
            return m.group(1)
        time.sleep(1)
    raise SystemExit("gateway did not start (no bootstrap token in gw.boot.log)")


SENSITIVE = load_sensitive(ACCOUNTS) if os.path.exists(ACCOUNTS) else set()
ADMIN_TOKEN = admin_token()
SENSITIVE.add(ADMIN_TOKEN)


def leaks(text):
    return sum(1 for v in SENSITIVE if v in text)


def check(name, ok, detail=""):
    line = ("PASS " if ok else "FAIL ") + name + ("  | " + detail if detail else "")
    if leaks(line):
        line = ("PASS " if ok else "FAIL ") + name + "  | [detail redacted: contained a secret]"
    results.append(bool(ok))
    printed.append(line)
    print(line, flush=True)


def req(method, url, token, body=None, headers=None, timeout=120):
    h = {"Authorization": "Bearer " + token, "Content-Type": "application/json"}
    h.update(headers or {})
    r = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(), headers=h, method=method)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def create_client(name, **extra):
    body = {"name": name, "role": "user", "quota_nano_usd": 10**11}
    body.update(extra)
    _, b = req("POST", ADMIN + "/admin/api/clients", ADMIN_TOKEN, body)
    tok = json.loads(b)["token"]
    SENSITIVE.add(tok)
    return tok


def import_keys(pool, *args):
    env = dict(os.environ, UAIG_ADMIN_URL=ADMIN, UAIG_ADMIN_TOKEN=ADMIN_TOKEN)
    p = subprocess.run([EXE, "keys", "import", "--pool", pool, "--file", ACCOUNTS, *args],
                       capture_output=True, text=True, encoding="utf-8", errors="replace", env=env, timeout=300)
    return p.returncode, p.stdout.strip(), leaks(p.stdout + p.stderr)


def main():
    code, out, leaked = import_keys("<池>", "--where", "status=ok", "--name-prefix", "t")
    check("import keys", code == 0 and not leaked, out)
    tok = create_client("live")
    st, body = req("GET", DATA + "/v1/models", tok)
    check("list services", st == 200, str(st))
    # …按卡片的验收步骤继续 check(...)


if __name__ == "__main__":
    main()
    log = open(os.path.join(HERE, "gw.boot.log"), encoding="utf-8", errors="replace").read()
    check("no secrets in gateway log", not leaks(log.replace(ADMIN_TOKEN, "")), f"{len(log)} bytes")
    print(f"{sum(results)}/{len(results)} passed", flush=True)
