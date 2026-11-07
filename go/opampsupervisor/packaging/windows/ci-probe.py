# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
"""CI only, temporary: what a Windows runner is doing when it stops answering.

CI's Windows installer job sometimes goes silent: the job dies at its limit with no log. This runs
apart from the runner and writes the machine's state to a check run of its own every 10 s. Unlike
the earlier heartbeat (a new HTTPS connection each time), it reports over connections it keeps
open, and checks separately whether DNS, a new TCP connection to GitHub, the Azure host's
wireserver (168.63.129.16) and loopback still work. So the last report tells a frozen machine from
one that can't open new connections, and shows the seconds before.

When reports stop getting through for 45 s, it goes on locally: it saves what the network looks like
(adapters, routes, ARP, the System log), tries to bring the network back one step at a time, and
reports again if one works. If none does, it saves everything, registers a task that reports the
saved file at the next boot, and restarts the machine. A frozen machine does none of this; a machine
that only lost its network says so after the restart.

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
import subprocess
import sys
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


def spawn():
    subprocess.run(['cmd.exe' if os.name == 'nt' else 'true'] + (['/c', 'exit'] if os.name == 'nt' else []), timeout=10, check=True)
    return ''


def scm():
    # The Service Control Manager, asked about one service: a wedged services.exe shows here.
    return psutil.win_service_get('EventLog').status() if os.name == 'nt' else ''


def disk_write():
    with open(os.path.join(os.environ.get('TEMP', '/tmp'), 'probe-write'), 'w') as f:
        f.write(now())
        f.flush()
        os.fsync(f.fileno())
    return ''


CHECKS = [Check('dns', dns), Check('tcp github', tcp('api.github.com', 443)),
          Check('tcp wireserver', tcp('168.63.129.16', 80)), Check('loopback', loopback),
          Check('spawn', spawn), Check('scm', scm), Check('fsync', disk_write)]


class Sampler:
    def __init__(self):
        self.procs: dict[int, psutil.Process] = {}
        self.last_disk = psutil.disk_io_counters(perdisk=True)
        self.services = self.service_states()
        self.changes: collections.deque[str] = collections.deque(maxlen=40)
        self.last_net = psutil.net_io_counters()
        self.last_t = time.monotonic()
        self.stage = ('start', time.monotonic())
        self.latest: tuple[str, list[str]] = ('', [])
        psutil.cpu_percent()

    def at(self, stage):
        self.stage = (stage, time.monotonic())

    def loop(self, stop):
        # Its own thread: a system call that hangs (the Service Control Manager, a process's
        # details) stops this, not the reports, which then say where it stopped.
        while not stop.is_set():
            try:
                self.latest = self.sample(True)
            except Exception as e:  # noqa: BLE001
                self.latest = (f'{now()} sample FAILED {type(e).__name__}: {e}', [])
            self.at('sleep')
            time.sleep(2)

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
        self.at('memory')
        vm = psutil.virtual_memory()
        sw = psutil.swap_memory()
        self.at('disk and net counters')
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
        self.at('services')
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
        self.at('processes')
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
        self.at('tcp connections')
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
        self.at('nics')
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
        self.last_ok = time.monotonic()
        self.thread = None

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
                self.last_ok = time.monotonic()
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

    def post_async(self, *args):
        # Off the main loop, so sampling (and noticing a dead network) goes on while a report hangs.
        if self.thread and self.thread.is_alive():
            return
        self.thread = threading.Thread(target=self.post, args=args, daemon=True)
        self.thread.start()

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


def sh(cmd, timeout=20):
    t = time.monotonic()
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, errors='replace', timeout=timeout, shell=isinstance(cmd, str))
        out = (r.stdout + r.stderr).rstrip()
        return f'$ {cmd} (exit {r.returncode}, {time.monotonic() - t:.1f}s)\n{out[-6000:]}'
    except Exception as e:  # noqa: BLE001
        return f'$ {cmd} FAILED after {time.monotonic() - t:.1f}s {type(e).__name__}: {e}'


def ps(script, timeout=30):
    return sh(['powershell', '-NoProfile', '-NonInteractive', '-Command', script], timeout)


DIAGNOSE = [
    lambda: sh('ipconfig /all'), lambda: sh('route print -4'), lambda: sh('arp -a'),
    lambda: ps('Get-NetAdapter | Format-Table -AutoSize Name, Status, LinkSpeed, MediaConnectionState, ifIndex | Out-String -Width 200'),
    lambda: ps('Get-NetIPInterface -AddressFamily IPv4 | Format-Table -AutoSize | Out-String -Width 200'),
    lambda: ps('Get-NetFirewallProfile | Format-Table Name, Enabled, DefaultInboundAction, DefaultOutboundAction | Out-String'),
    lambda: ps('Get-NetTCPConnection -State Established,SynSent | Format-Table -AutoSize LocalAddress, LocalPort, RemoteAddress, RemotePort, State, OwningProcess | Out-String -Width 200'),
    lambda: sh('wevtutil qe System /c:60 /rd:true /f:text', 40),
    lambda: sh('wevtutil qe Application /c:30 /rd:true /f:text', 40),
]

RECOVER = [
    ('wait', lambda: 'waited'),
    ('ipconfig /renew', lambda: sh('ipconfig /renew', 60)),
    ('flush ARP and DNS', lambda: sh('netsh interface ip delete arpcache') + '\n' + sh('ipconfig /flushdns')),
    ('stop the Supervisor service', lambda: ps('Stop-Service pydantic-opamp-supervisor -Force -ErrorAction Continue; Get-Process otelcol*,pydantic-opamp* -ErrorAction SilentlyContinue | Stop-Process -Force; "done"', 60)),
    ('restart the NIC', lambda: ps("Get-NetAdapter -Physical | Restart-NetAdapter -Confirm:$false; 'done'", 90)),
]


def after_boot(a):
    # Runs from the boot task: reports what the probe saved before it restarted the machine.
    token = open(a.token_file).read().strip()
    saved = json.load(open(a.dark, encoding='utf-8'))
    rep = Reporter(a.repo, a.sha, a.name, token, False)
    rep.id = saved['id']
    for _ in range(60):
        rep.post('completed', f"went dark, restarted, reported after boot at {now()} UTC",
                 f'booted again {datetime.fromtimestamp(psutil.boot_time(), timezone.utc).isoformat()}\n' + saved['text'], 'failure')
        if time.monotonic() - rep.last_ok < 5:
            return
        time.sleep(10)


def go_dark(a, rep, ring, checks, out):
    """The reports stopped getting through: find out locally whether it's the network, and try to
    get it back."""
    lines = [f'{now()} reports stopped getting through ({time.monotonic() - rep.last_ok:.0f}s since one did)',
             *checks, '## Reporting', *rep.log, '## Every 2 s', *ring]

    def save():
        text = '\n'.join(lines)
        with open(a.dark, 'w', encoding='utf-8') as f:
            json.dump({'id': rep.id, 'text': text}, f)
        if out:
            out.write(text + '\n')
            out.flush()

    save()
    lines.append('## Diagnosis')
    for d in DIAGNOSE:
        lines.append(d())
        save()
    for name, step in RECOVER:
        lines.append(f'## {now()} recovery: {name}')
        lines.append(step())
        time.sleep(20)
        lines.append(f'{now()} ' + ' | '.join(c.run() for c in CHECKS))
        save()
        rep.post('in_progress', f'went dark, back after "{name}" at {now()} UTC', '\n'.join(lines))
        if time.monotonic() - rep.last_ok < 30:
            return True
    lines.append(f'## {now()} nothing brought it back: restarting the machine to report after boot')
    save()
    task = (f'"{sys.executable}" "{os.path.abspath(__file__)}" --after-boot --repo {a.repo} --sha {a.sha} '
            f'--name "{a.name}" --log x --stop x --dark "{a.dark}" --token-file "{a.token_file}"')
    lines.append(sh(['schtasks', '/Create', '/F', '/RU', 'SYSTEM', '/SC', 'ONSTART', '/TN', 'probe-after-boot', '/TR', task]))
    save()
    lines.append(sh('shutdown /r /f /t 5'))
    save()
    time.sleep(600)
    return False


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
    ap.add_argument('--dark', help='where to save the state when reports stop getting through')
    ap.add_argument('--after-boot', action='store_true')
    a = ap.parse_args()
    if a.after_boot:
        return after_boot(a)
    boot = datetime.fromtimestamp(psutil.boot_time(), timezone.utc).isoformat()
    token = open(a.token_file).read().strip() if a.token_file else os.environ.get('HEARTBEAT_TOKEN', '')
    rep = Reporter(a.repo, a.sha, a.name, token, a.dry_run)
    sampler = Sampler()
    ring: collections.deque[str] = collections.deque(maxlen=60)
    checks: collections.deque[str] = collections.deque(maxlen=25)
    out = open(a.out, 'a', encoding='utf-8') if a.out else None
    deadline = time.monotonic() + 60 * a.minutes
    stop = threading.Event()
    threading.Thread(target=sampler.loop, args=(stop,), daemon=True).start()
    tick = 0
    last_line = ''
    while time.monotonic() < deadline and not os.path.exists(a.stop):
        tick += 1
        time.sleep(3)
        line, detail = sampler.latest
        stage, since = sampler.stage
        stuck = time.monotonic() - since
        if line != last_line:
            ring.append(line)
            last_line = line
            if out:
                out.write(line + '\n')
                out.flush()
        results = [c.run() for c in CHECKS]
        sampler_state = f'sampler: {stage} for {stuck:.0f}s' + (' STUCK' if stage != 'sleep' and stuck > 10 else '')
        checks.append(f'{now()} {sampler_state} | ' + ' | '.join(results))
        if out:
            out.write(checks[-1] + '\n')
            out.flush()
        if tick % 2 == 1:
            text = '\n'.join([f'booted {boot}', '## Checks (every 3 s)', *checks,
                              '## Reporting', *rep.log, '## Latest sample', *detail,
                              '## Every 2 s (last 3 min)', *ring, '## Newest MSI log', *[l[:300] for l in msi_tail()],
                              '## The test, last 40 lines', *[l.rstrip() for l in tail(a.log, 40)]])
            title = f'alive at {now()} UTC' + (f', sampler stuck in {stage} {stuck:.0f}s' if 'STUCK' in sampler_state else '')
            rep.post_async('in_progress', title, text)
        if a.dark and rep.id is not None and time.monotonic() - rep.last_ok > 45:
            if rep.thread:
                rep.thread.join(30)
            go_dark(a, rep, ring, checks, out)
            rep.last_ok = time.monotonic()
        if tick % 15 == 0 and not (rep.thread and rep.thread.is_alive()):
            rep.keepalive()
    stop.set()
    done = os.path.exists(a.stop)
    rep.post('completed', 'the job finished' if done else f"the job didn't finish in {a.minutes} minutes",
             '\n'.join([*checks, *rep.log, *ring]), 'success' if done else 'failure')


if __name__ == '__main__':
    main()
