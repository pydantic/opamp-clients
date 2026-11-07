# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""CI only, temporary: what a Windows runner is doing when it stops answering.

CI's Windows installer job sometimes goes silent: the job dies at its limit with no log. This runs
apart from the runner and writes the machine's state to a check run of its own every 10 s. Unlike
the earlier heartbeat (a new HTTPS connection each time), it reports over connections it keeps
open, and checks separately whether DNS, a new TCP connection to GitHub, the Azure host's
wireserver (168.63.129.16) and loopback still work. So the last report tells a frozen machine from
one that can't open new connections, and shows the seconds before.

  python ci-probe.py --repo O/R --sha SHA --name NAME --log FILE --stop FILE [--dry-run]

--token-file holds the token that writes check runs (HEARTBEAT_TOKEN otherwise).
"""

from __future__ import annotations

import argparse
import collections
import http.client
import json
import os
import socket
import ssl
import threading
import time
from datetime import datetime, timezone

import psutil

HANDLES = 'num_handles' if os.name == 'nt' else 'num_fds'
OURS = ('opamp', 'otelcol', 'msiexec', 'install.test', 'powershell', 'pwsh', 'runner', 'bash', 'conhost', 'go.exe', 'python')


def now() -> str:
    return datetime.now(timezone.utc).strftime('%H:%M:%S.%f')[:-3]


class Check:
    """A network check run in a thread of its own, so a hang (getaddrinfo has no timeout) shows as
    'pending since' instead of stopping the probe."""

    def __init__(self, name, fn):
        self.name, self.fn = name, fn
        self.thread = None
        self.started = 0.0
        self.result = 'not run'

    def run(self):
        if self.thread and self.thread.is_alive():
            return f'{self.name}: PENDING {time.monotonic() - self.started:.0f}s'
        self.started = time.monotonic()

        def go():
            t = time.monotonic()
            try:
                detail = self.fn()
                self.result = f'{self.name}: ok {1000 * (time.monotonic() - t):.0f}ms {detail}'
            except Exception as e:  # noqa: BLE001
                self.result = f'{self.name}: FAIL {1000 * (time.monotonic() - t):.0f}ms {type(e).__name__}: {e}'

        self.thread = threading.Thread(target=go, daemon=True)
        self.thread.start()
        self.thread.join(4)
        return self.result if not self.thread.is_alive() else f'{self.name}: PENDING'


def dns():
    return sorted({a[4][0] for a in socket.getaddrinfo('api.github.com', 443, type=socket.SOCK_STREAM)})[0]


def tcp(host, port):
    def fn():
        with socket.create_connection((host, port), timeout=3) as s:
            return str(s.getsockname()[1])

    return fn


def loopback():
    srv = socket.socket()
    srv.bind(('127.0.0.1', 0))
    srv.listen(1)
    try:
        with socket.create_connection(srv.getsockname(), timeout=3):
            return ''
    finally:
        srv.close()


CHECKS = [Check('dns', dns), Check('tcp github', tcp('api.github.com', 443)),
          Check('tcp wireserver', tcp('168.63.129.16', 80)), Check('loopback', loopback)]


class Sampler:
    def __init__(self):
        self.procs: dict[int, psutil.Process] = {}
        self.last_disk = psutil.disk_io_counters(perdisk=True)
        self.services = self.service_states()
        self.changes: collections.deque[str] = collections.deque(maxlen=40)
        self.last_net = psutil.net_io_counters()
        self.last_t = time.monotonic()
        psutil.cpu_percent()

    @staticmethod
    def service_states():
        out = {}
        if os.name != 'nt':
            return out
        for svc in psutil.win_service_iter():
            try:
                d = svc.as_dict()
            except Exception:  # noqa: BLE001
                continue
            out[d['name']] = (d['status'], d['pid'])
        return out

    def sample(self, detailed: bool) -> tuple[str, list[str]]:
        t = time.monotonic()
        dt = max(t - self.last_t, 0.001)
        self.last_t = t
        vm = psutil.virtual_memory()
        sw = psutil.swap_memory()
        disk = psutil.disk_io_counters(perdisk=True)
        net = psutil.net_io_counters()
        disks = []
        for name, d in disk.items():
            o = self.last_disk.get(name)
            if not o:
                continue
            ops = (d.read_count - o.read_count) + (d.write_count - o.write_count)
            busy = (d.read_time - o.read_time) + (d.write_time - o.write_time)
            disks.append(f'{name[-1]}:r{(d.read_bytes - o.read_bytes) / dt / 1e6:.0f}w{(d.write_bytes - o.write_bytes) / dt / 1e6:.0f}MB/s '
                         f'{ops / dt:.0f}iops {busy / max(ops, 1):.0f}ms/op')
        nr = (net.bytes_recv - self.last_net.bytes_recv) / dt / 1e3
        ns = (net.bytes_sent - self.last_net.bytes_sent) / dt / 1e3
        self.last_disk, self.last_net = disk, net
        services = self.service_states()
        for name, (status, pid) in services.items():
            old = self.services.get(name)
            if old and old[0] != status:
                self.changes.append(f'{now()} service {name} {old[0]} -> {status} (pid {pid})')
            elif not old:
                self.changes.append(f'{now()} service {name} added {status}')
        for name in set(self.services) - set(services):
            self.changes.append(f'{now()} service {name} removed')
        self.services = services
        svc_by_pid: dict[int, list[str]] = collections.defaultdict(list)
        for name, (status, pid) in services.items():
            if pid:
                svc_by_pid[pid].append(name)
        rows = []
        seen = set()
        for p in psutil.process_iter(['name', 'ppid', 'memory_info', HANDLES, 'num_threads', 'cmdline']):
            seen.add(p.pid)
            if p.pid == 0:  # Windows' System Idle Process: its CPU is the idle time
                continue
            cached = self.procs.setdefault(p.pid, p)
            try:
                cpu = cached.cpu_percent()
            except psutil.Error:
                continue
            rows.append((cpu, p))
        for pid in list(self.procs):
            if pid not in seen:
                del self.procs[pid]
        try:
            conns = psutil.net_connections('tcp')
        except psutil.Error as e:
            conns = []
            conn_err = str(e)
        else:
            conn_err = ''
        states = collections.Counter(c.status for c in conns)
        by_pid = collections.Counter(c.pid for c in conns)
        top_cpu = sorted(rows, key=lambda r: -r[0])[:4]

        def label(p):
            svcs = svc_by_pid.get(p.pid)
            return p.info['name'] + (f'[{",".join(svcs)[:60]}]' if svcs else '')
        line = (f'{now()} cpu {psutil.cpu_percent():.0f}% mem avail {vm.available >> 20}MB swap {sw.percent:.0f}% '
                f'disk {" ".join(disks)} net r{nr:.0f} s{ns:.0f}KB/s tcp {len(conns)} '
                f'{dict(states)} procs {len(rows)} top ' +
                ', '.join(f'{label(p)}:{c:.0f}%' for c, p in top_cpu))
        if not detailed:
            return line, []
        names = {p.pid: p.info['name'] for _, p in rows}
        detail = [line, conn_err, '## TCP connections by process (top 8)']
        detail += [f'{pid} {names.get(pid, "?")}: {n}' for pid, n in by_pid.most_common(8)]
        detail.append('## Ours, msiexec, PowerShell, the runner')
        for c, p in sorted(rows, key=lambda r: r[1].pid):
            name = (p.info['name'] or '').lower()
            if any(o in name for o in OURS):
                mi = p.info['memory_info']
                cmd = ' '.join(p.info['cmdline'] or [])[:300]
                detail.append(f'{p.pid} <- {p.info["ppid"]} {p.info["name"]} cpu {c:.0f}% rss {(mi.rss >> 20) if mi else "?"}MB '
                              f'handles {p.info[HANDLES]} threads {p.info["num_threads"]}: {cmd}')
        detail.append('## Most CPU')
        detail += [f'{p.pid} {label(p)} {c:.0f}%' for c, p in sorted(rows, key=lambda r: -r[0])[:10]]
        detail.append('## Service changes')
        detail += list(self.changes)
        detail.append('## Most memory')
        mem = sorted(rows, key=lambda r: -(r[1].info['memory_info'].rss if r[1].info['memory_info'] else 0))[:8]
        detail += [f'{p.pid} {p.info["name"]} {p.info["memory_info"].rss >> 20}MB' for _, p in mem]
        detail.append('## Most handles')
        hnd = sorted(rows, key=lambda r: -(r[1].info[HANDLES] or 0))[:6]
        detail += [f'{p.pid} {p.info["name"]} {p.info[HANDLES]}' for _, p in hnd]
        try:
            for nic, st in psutil.net_if_stats().items():
                detail.append(f'nic {nic}: up {st.isup} speed {st.speed}')
        except psutil.Error:
            pass
        return line, detail


class Reporter:
    """Writes the check run over kept-open connections: the main one, then a spare opened at the
    start (so a machine that can't open new connections still reports), then a new one."""

    def __init__(self, repo, sha, name, token, dry_run):
        self.repo, self.sha, self.name, self.dry = repo, sha, name, dry_run
        self.headers = {'Authorization': f'Bearer {token}', 'Accept': 'application/vnd.github+json',
                        'User-Agent': 'opamp-clients-ci-probe', 'Content-Type': 'application/json'}
        self.ctx = ssl.create_default_context()
        self.conns = [self.connect(), self.connect()] if not dry_run else []
        self.id = None
        self.log: collections.deque[str] = collections.deque(maxlen=30)

    def connect(self):
        c = http.client.HTTPSConnection('api.github.com', timeout=10, context=self.ctx)
        c.connect()
        return c

    def request(self, method, path, body):
        if self.dry:
            print(method, path, json.dumps(body)[:1500])
            return {'id': 1}
        data = json.dumps(body)
        for i in range(len(self.conns) + 1):
            if i == len(self.conns):
                try:
                    self.conns.append(self.connect())
                    self.log.append(f'{now()} opened a new connection')
                except Exception as e:  # noqa: BLE001
                    self.log.append(f'{now()} new connection FAILED {type(e).__name__}: {e}')
                    return None
            c = self.conns[i]
            try:
                c.request(method, path, data, self.headers)
                r = c.getresponse()
                out = r.read()
                if r.status >= 300:
                    self.log.append(f'{now()} conn {i}: HTTP {r.status} {out[:200]!r}')
                    return None
                if i:
                    self.log.append(f'{now()} sent on conn {i}')
                return json.loads(out)
            except Exception as e:  # noqa: BLE001
                self.log.append(f'{now()} conn {i} FAILED {type(e).__name__}: {e}')
                c.close()
                try:
                    c.connect()
                except Exception:  # noqa: BLE001
                    pass
        return None

    def post(self, status, title, text, conclusion=None):
        body = {'status': status, 'output': {'title': title, 'summary': title, 'text': '```\n' + text[-60000:] + '\n```'}}
        if conclusion:
            body['conclusion'] = conclusion
        if self.id is None:
            body.update(name=self.name, head_sha=self.sha)
            r = self.request('POST', f'/repos/{self.repo}/check-runs', body)
            if r:
                self.id = r['id']
        else:
            self.request('PATCH', f'/repos/{self.repo}/check-runs/{self.id}', body)

    def keepalive(self):
        # Keeps the spare connection from idling out: a cheap call that doesn't count against the
        # rate limit.
        if self.dry or len(self.conns) < 2:
            return
        try:
            self.conns[1].request('GET', '/rate_limit', headers=self.headers)
            self.conns[1].getresponse().read()
        except Exception as e:  # noqa: BLE001
            self.log.append(f'{now()} spare keepalive FAILED {type(e).__name__}: {e}')
            self.conns[1].close()


def msi_tail(n=25):
    # With the Logging policy set (the workflow sets it) every msiexec writes MSI*.LOG in TEMP: the
    # newest one's last lines, and what Restart Manager decided.
    import glob
    logs = []
    for d in (os.environ.get('TEMP', ''), r'C:\Windows\Temp'):
        logs += glob.glob(os.path.join(d, 'MSI*.LOG'))
    if not logs:
        return ['(no MSI logs)']
    newest = max(logs, key=os.path.getmtime)
    try:
        raw = open(newest, 'rb').read()
    except OSError as e:
        return [str(e)]
    text = raw.decode('utf-16', errors='replace') if raw[:2] == b'\xff\xfe' else raw.decode('utf-8', errors='replace')
    lines = text.splitlines()
    rm = [l for l in lines if 'RESTART MANAGER' in l or 'Return value 3' in l or 'Note: 1: 1' in l][-10:]
    return [f'{newest} ({len(raw)} bytes)', *rm, '...', *lines[-n:]]


def tail(path, n=60):
    try:
        with open(path, encoding='utf-8', errors='replace') as f:
            return f.readlines()[-n:]
    except OSError as e:
        return [str(e)]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--repo', required=True)
    ap.add_argument('--sha', required=True)
    ap.add_argument('--name', required=True)
    ap.add_argument('--log', required=True)
    ap.add_argument('--stop', required=True)
    ap.add_argument('--out', help='also append every sample here')
    ap.add_argument('--minutes', type=float, default=40)
    ap.add_argument('--dry-run', action='store_true')
    ap.add_argument('--token-file')
    a = ap.parse_args()
    boot = datetime.fromtimestamp(psutil.boot_time(), timezone.utc).isoformat()
    token = open(a.token_file).read().strip() if a.token_file else os.environ.get('HEARTBEAT_TOKEN', '')
    rep = Reporter(a.repo, a.sha, a.name, token, a.dry_run)
    sampler = Sampler()
    ring: collections.deque[str] = collections.deque(maxlen=60)
    checks: collections.deque[str] = collections.deque(maxlen=15)
    out = open(a.out, 'a', encoding='utf-8') if a.out else None
    deadline = time.monotonic() + 60 * a.minutes
    tick = 0
    while time.monotonic() < deadline and not os.path.exists(a.stop):
        tick += 1
        report = tick % 2 == 1  # every 6 s or so
        line, detail = sampler.sample(report)
        ring.append(line)
        if out:
            out.write(line + '\n')
            out.flush()
        if report:
            results = [c.run() for c in CHECKS]
            checks.append(f'{now()} ' + ' | '.join(results))
            text = '\n'.join([f'booted {boot}', '## Network checks (every report)', *checks,
                              '## Reporting', *rep.log, '## Now', *detail,
                              '## Every 2 s (last 3 min)', *ring, '## Newest MSI log', *[l[:300] for l in msi_tail()],
                              '## The test, last 40 lines', *[l.rstrip() for l in tail(a.log, 40)]])
            if out:
                out.write('\n'.join(['--- ' + checks[-1], *detail]) + '\n')
                out.flush()
            rep.post('in_progress', f'alive at {now()} UTC', text)
        if tick % 20 == 0:
            rep.keepalive()
        time.sleep(2)
    done = os.path.exists(a.stop)
    rep.post('completed', 'the job finished' if done else f"the job didn't finish in {a.minutes} minutes",
             '\n'.join([*checks, *rep.log, *ring]), 'success' if done else 'failure')


if __name__ == '__main__':
    main()
