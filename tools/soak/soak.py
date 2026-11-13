"""Bounded local HTTP soak driver; never contacts a real provider."""
import argparse
import bisect
import csv
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import queue
import re
import secrets
import signal
import sqlite3
import subprocess
import threading
import time

BOUNDS = [.001, .002, .005, .01, .025, .05, .1, .25, .5, 1, 2, 5, 10, 30]
MAX_BODY = 1 << 20

class Upstream(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def log_message(self, *_):
        pass
    def setup(self):
        super().setup()
        self.connection.settimeout(5)
    def do_GET(self):
        self.respond(b'{"ok":true}')
    def do_POST(self):
        n = int(self.headers.get('Content-Length', '0'))
        if n < 0 or n > MAX_BODY:
            self.send_error(413)
            return
        body = self.rfile.read(n)
        if self.path == '/v1/messages':
            self.respond(b'{"id":"msg_soak","type":"message","role":"assistant","model":"up-anth","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}')
        elif self.path == '/v1/chat/completions':
            data = json.loads(body)
            if data.get('stream'):
                self.respond(b'data: {"id":"soak","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}\n\ndata: {"id":"soak","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}\n\ndata: [DONE]\n\n', 'text/event-stream')
            else:
                self.respond(b'{"id":"soak","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}')
        else:
            self.respond(b'{"ok":true}')
    def respond(self, body, content_type='application/json'):
        self.send_response(200)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def request(address, method, path, token='', body=None, connection=None):
    own = connection is None
    conn = connection or http.client.HTTPConnection(address, timeout=5)
    raw = None if body is None else json.dumps(body).encode()
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    try:
        conn.request(method, path, raw, headers)
        response = conn.getresponse()
        result = response.read(MAX_BODY + 1)
        if len(result) > MAX_BODY:
            raise RuntimeError('response_limit')
        return response.status, result
    finally:
        if own:
            conn.close()


def atomic_json(path, value):
    temp = path.with_suffix('.tmp')
    temp.write_text(json.dumps(value, indent=2, sort_keys=True), encoding='utf-8')
    os.chmod(temp, 0o600)
    os.replace(temp, path)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', required=True)
    parser.add_argument('--output', required=True)
    parser.add_argument('--seconds', type=float, default=86400)
    parser.add_argument('--source-commit', default='unknown')
    parser.add_argument('--source-patch-sha256', default='none')
    parser.add_argument('--sample-every', type=float, default=10)
    parser.add_argument('--reload-every', type=float, default=300)
    args = parser.parse_args()
    if not 1 <= args.seconds <= 86400 or not 1 <= args.sample_every <= 60 or args.reload_every < 5:
        raise SystemExit('invalid duration or interval')
    os.umask(0o077)
    out = Path(args.output)
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    stop = threading.Event()
    aborted = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: (aborted.set(), stop.set()))
    up = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    up.daemon_threads = True
    up_thread = threading.Thread(target=up.serve_forever, daemon=True)
    up_thread.start()
    config = out / 'gateway.yaml'
    source = {'commit': args.source_commit,
              'patch_sha256': args.source_patch_sha256,
              'binary_sha256': hashlib.sha256(Path(args.binary).read_bytes()).hexdigest()}
    def config_text(strategy):
        lines = ['server:', '  data_listen: "127.0.0.1:0"', '  admin_listen: "127.0.0.1:0"',
                 '  data_dir: ' + json.dumps(str(out / 'data')), '  master_key_file: ' + json.dumps(str(out / 'secrets/master.key')),
                 '  shutdown_grace: 5s', '  log_level: info', 'limits:', '  preauth_ip_rpm: 0', 'pools:']
        for name, protocol in [('chat', 'openai_chat'), ('anth', 'anthropic'), ('rest', 'rest')]:
            lines += ['  - name: ' + name, '    protocol: ' + protocol,
                      '    base_url: "http://127.0.0.1:' + str(up.server_port) + '"', '    strategy: ' + strategy]
        lines += ['models:', '  - name: chat', '    pool: chat', '    upstream_model: up-chat',
                  '    price: {input_tokens: "1/1M", output_tokens: "1/1M", cache_read_tokens: "0"}',
                  '    reserve: {output_tokens: 64}', '  - name: translated', '    pool: anth', '    upstream_model: up-anth',
                  '    price: {input_tokens: "1/1M", output_tokens: "1/1M", cache_read_tokens: "0", cache_write_tokens: "0"}',
                  '    reserve: {output_tokens: 64}', '  - name: rest', '    pool: rest',
                  '    price: {requests: "0.001"}', '    reserve: {requests: 1}']
        return '\n'.join(lines) + '\n'
    config.write_text(config_text('weighted_round_robin'))
    gateway = subprocess.Popen([args.binary, 'serve', '--config', str(config)], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    ready = queue.Queue(maxsize=1)
    startup = {}
    log_errors = []
    def drain_logs():
        with (out / 'gateway_warnings.jsonl').open('w') as log:
            for line in iter(gateway.stderr.readline, ''):
                if line.startswith('BOOTSTRAP ADMIN TOKEN (shown once): '):
                    startup['token'] = line.partition(': ')[2].strip()
                else:
                    try:
                        entry = json.loads(line)
                    except ValueError:
                        continue
                    if entry.get('msg') == 'gateway listening':
                        startup.update(data=entry['data'], admin=entry['admin'])
                    if entry.get('level') in ('WARN', 'ERROR'):
                        text = json.dumps({'time': entry.get('time'), 'level': entry['level'], 'msg': entry.get('msg')})
                        log.write(text + '\n')
                        log.flush()
                        if len(log_errors) < 20:
                            log_errors.append(entry.get('msg'))
                        if log.tell() > 10 * (1 << 20):
                            stop.set()
                if all(k in startup for k in ('token', 'data', 'admin')) and ready.empty():
                    ready.put(dict(startup))
    drainer = threading.Thread(target=drain_logs, daemon=True)
    drainer.start()
    workers = []
    counts = [0] * 4
    failures = [0] * 4
    histogram = [0] * (len(BOUNDS) + 1)
    lock = threading.Lock()
    samples = []
    started = time.time()
    monotonic_start = None
    reloads = 0
    failure_reason = None
    status = 'failed'
    client_id = None
    resources = None
    try:
        addresses = ready.get(timeout=15)
        admin, data, admin_token = addresses['admin'], addresses['data'], addresses['token']
        code, raw = request(admin, 'POST', '/admin/api/clients', admin_token,
                            {'name': 't61-soak-client', 'role': 'user', 'quota_unlimited': True, 'rpm_limit': 0})
        if code != 201:
            raise RuntimeError('create_client_http_' + str(code))
        created = json.loads(raw)
        token, client_id = created['token'], created['client']['id']
        for pool in ('chat', 'anth', 'rest'):
            for i, weight in enumerate((5, 1)):
                code, _ = request(admin, 'POST', '/admin/api/keys', admin_token,
                                  {'pool': pool, 'name': pool + '-' + str(i), 'secret': ('bearer:' if pool == 'rest' else '') + secrets.token_urlsafe(24), 'weight': weight, 'max_in_flight': 32})
                if code != 201:
                    raise RuntimeError('create_key_http_' + str(code))
        workloads = [
            ('POST', '/v1/chat/completions', {'model': 'chat', 'messages': [{'role': 'user', 'content': 't61 synthetic ' * 80}], 'max_tokens': 16}),
            ('POST', '/v1/chat/completions', {'model': 'chat', 'messages': [{'role': 'user', 'content': 't61 synthetic'}], 'max_tokens': 16, 'stream': True}),
            ('POST', '/v1/chat/completions', {'model': 'translated', 'messages': [{'role': 'user', 'content': 't61 synthetic'}], 'max_tokens': 16}),
            ('GET', '/rest/rest/health', None),
        ]
        def worker(i):
            conn = http.client.HTTPConnection(data, timeout=5)
            try:
                while not stop.is_set():
                    begin = time.monotonic()
                    try:
                        code, body = request(data, *workloads[i][:2], token, workloads[i][2], conn)
                        valid = code == 200 and (b'[DONE]' in body if i == 1 else b'ok' in body)
                    except (OSError, http.client.HTTPException, RuntimeError):
                        valid = False
                        conn.close()
                        conn = http.client.HTTPConnection(data, timeout=5)
                    elapsed = time.monotonic() - begin
                    with lock:
                        counts[i] += 1
                        failures[i] += int(not valid)
                        histogram[bisect.bisect_left(BOUNDS, elapsed)] += 1
                    if not valid:
                        stop.set()
                    stop.wait(max(0, 2 - elapsed))
            finally:
                conn.close()
        monotonic_start = time.monotonic()
        for i in range(4):
            thread = threading.Thread(target=worker, args=(i,))
            workers.append(thread)
            thread.start()
        atomic_json(out / 'status.json', {'status': 'running', 'pid': os.getpid(), 'gateway_pid': gateway.pid,
                    'started_at': started, 'duration_seconds': args.seconds, 'source': source, 'requests_per_second_target': 2})
        next_reload = monotonic_start + args.reload_every
        with (out / 'samples.csv').open('w', newline='') as stream:
            writer = csv.DictWriter(stream, fieldnames=['elapsed_seconds', 'rss_kib', 'goroutines', 'requests', 'failures', 'ledger_queue', 'audit_dropped', 'data_bytes'])
            writer.writeheader()
            while not stop.is_set() and time.monotonic() - monotonic_start < args.seconds:
                if gateway.poll() is not None:
                    raise RuntimeError('gateway_exited')
                code, body = request(admin, 'GET', '/admin/metrics')
                if code != 200:
                    raise RuntimeError('metrics_http_' + str(code))
                metrics = {}
                for line in body.decode().splitlines():
                    if re.match(r'^uaig_[a-z_]+ ', line):
                        k, v = line.split(' ', 1)
                        metrics[k] = float(v)
                proc = Path('/proc/' + str(gateway.pid) + '/status').read_text()
                rss = int(re.search(r'^VmRSS:\s+(\d+)', proc, re.M)[1])
                size = sum(p.stat().st_size for p in (out / 'data').iterdir() if p.is_file())
                with lock:
                    sample = {'elapsed_seconds': round(time.monotonic() - monotonic_start, 3), 'rss_kib': rss,
                              'goroutines': int(metrics['uaig_goroutines']), 'requests': sum(counts), 'failures': sum(failures),
                              'ledger_queue': metrics.get('uaig_ledger_events_queue_length'), 'audit_dropped': metrics.get('uaig_audit_dropped_total'), 'data_bytes': size}
                samples.append(sample)  # At most 86401 bounded samples (default: 8641).
                writer.writerow(sample)
                stream.flush()
                atomic_json(out / 'latest.json', sample)
                if rss > 768 * 1024 or sample['goroutines'] > 512 or size > 1024 * (1 << 20):
                    raise RuntimeError('resource_limit')
                if time.monotonic() >= next_reload:
                    strategy = 'least_inflight' if reloads % 2 == 0 else 'weighted_round_robin'
                    temp = out / 'config.next'
                    temp.write_text(config_text(strategy))
                    os.replace(temp, config)
                    code, _ = request(admin, 'POST', '/admin/api/reload', admin_token, {})
                    if code != 200:
                        raise RuntimeError('reload_http_' + str(code))
                    reloads += 1
                    next_reload += args.reload_every
                stop.wait(min(args.sample_every, max(0, args.seconds - (time.monotonic() - monotonic_start))))
        if aborted.is_set():
            failure_reason = 'interrupted'
        elif sum(failures):
            failure_reason = 'unexpected_data_response'
        elif time.monotonic() - monotonic_start < args.seconds:
            failure_reason = 'stopped_early'
        else:
            status = 'passed'
    except Exception as error:
        failure_reason = type(error).__name__ + ':' + (str(error) if isinstance(error, RuntimeError) else 'driver_failure')
    finally:
        stop.set()
        for thread in workers:
            thread.join(timeout=6)
        if gateway.poll() is None:
            gateway.terminate()
            try:
                gateway.wait(timeout=15)
            except subprocess.TimeoutExpired:
                gateway.kill()
                gateway.wait(timeout=5)
                failure_reason = failure_reason or 'shutdown_timeout'
                status = 'failed'
        drainer.join(timeout=5)
        up.shutdown()
        up.server_close()
        up_thread.join(timeout=5)
    persisted = None
    if client_id is not None:
        try:
            db = sqlite3.connect('file:' + str(out / 'data/gateway.db') + '?mode=ro', uri=True, timeout=5)
            persisted = db.execute('SELECT COUNT(*) FROM usage_events WHERE client_id=?', (client_id,)).fetchone()[0]
            db.close()
            if persisted != sum(counts) - sum(failures):
                status, failure_reason = 'failed', 'persisted_usage_count_mismatch'
        except sqlite3.Error:
            status, failure_reason = 'failed', 'database_verification_failed'
    if gateway.returncode != 0:
        status, failure_reason = 'failed', failure_reason or 'gateway_nonzero_exit'
    summary = {'status': status, 'failure_reason': failure_reason, 'started_at': started, 'ended_at': time.time(),
               'load_seconds': None if monotonic_start is None else time.monotonic() - monotonic_start,
               'configured_seconds': args.seconds, 'source': source, 'counts': counts, 'failures': failures,
               'latency_bucket_bounds_seconds': BOUNDS, 'latency_bucket_counts': histogram,
               'reloads': reloads, 'persisted_usage_events': persisted, 'sample_count': len(samples),
               'peak_rss_kib': max((s['rss_kib'] for s in samples), default=None),
               'peak_goroutines': max((s['goroutines'] for s in samples), default=None), 'gateway_warning_messages': log_errors}
    atomic_json(out / 'summary.json', summary)
    atomic_json(out / 'status.json', {'status': status, 'ended_at': time.time(), 'source': source})
    print(json.dumps({k: summary[k] for k in ('status', 'failure_reason', 'counts', 'failures', 'persisted_usage_events', 'reloads')}), flush=True)
    return 0 if status == 'passed' else 1

if __name__ == '__main__':
    raise SystemExit(main())
