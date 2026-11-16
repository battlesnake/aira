#!/usr/bin/env python3
"""AIRA-324: what an admission daemon that predates `discountable` answers.

Sends two raw admit frames carrying `discountable: true` to the live daemon
socket and prints the answers:

  minimal  {slice, reserve, pinned, discountable, scope_id, name, owner,
            max_wait_ms}                                       (8 fields)
  largest  the largest frame a discountable client can send: the minimal
           frame plus cpu, signature, exclusive_holder and priority (12),
           plus max_wait_ms                                    (13 fields)

Both stay under the pre-324 daemon's 15-field count guard, so both must reach
the allow-list and be refused with exactly
`E_DAEMON_PROTOCOL: unexpected admit field "discountable"` (the text the
client's once-only re-ask matches, runner.oldDaemonDiscountRefusal). The
allow-list runs in validateAdmitArgs, the first thing admitConnection does, so
a refused frame never reaches the enqueue.

Each frame also carries max_wait_ms=0 (non-blocking) and a 1 MiB reserve, so a
daemon that DID accept it (an AIRA-324 daemon) answers at once and the 1 MiB
lease is released when the probe closes the socket. Do not run it against an
AIRA-324 daemon all the same: there it proves nothing.

A point-in-time record of the AIRA-324 PR. Measured 2026-11-15 against the
live proto-15 daemon at revision 97f3440f: both frames printed `-> as expected`.

Usage: python3 docs/dev/aira-324-old-daemon-probe.py [socket]
"""
import json
import os
import pathlib
import socket
import struct
import sys
import time

EXPECTED = 'E_DAEMON_PROTOCOL: unexpected admit field "discountable"'


def default_socket() -> str:
    runtime = pathlib.Path(os.environ.get("XDG_RUNTIME_DIR", f"/run/user/{os.getuid()}")) / "aira"
    sockets = sorted(runtime.glob("*/daemon.sock"))
    if len(sockets) != 1:
        sys.exit(f"cannot choose a daemon socket under {runtime}: {sockets}")
    return str(sockets[0])


def exchange(path: str, args: dict) -> dict:
    frame = {"proto": 15, "scope": {}, "request": {"verb": "admit", "args": args, "has_content": False}}
    payload = json.dumps(frame).encode()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(5)
        conn.connect(path)
        conn.sendall(struct.pack(">I", len(payload)) + payload)
        header = conn.recv(4, socket.MSG_WAITALL)
        (size,) = struct.unpack(">I", header)
        body = b""
        while len(body) < size:
            chunk = conn.recv(size - len(body))
            if not chunk:
                break
            body += chunk
    return json.loads(body)


def scope_id(name: str) -> str:
    # The canonical confine scope-id grammar: CONFINE-<name>-<pid>-<base36 stamp>.
    stamp = int(time.time_ns())
    digits = "0123456789abcdefghijklmnopqrstuvwxyz"
    encoded = ""
    while stamp:
        stamp, rem = divmod(stamp, 36)
        encoded = digits[rem] + encoded
    return f"CONFINE-{name}-{os.getpid()}-{encoded}"


def main() -> int:
    sock = sys.argv[1] if len(sys.argv) > 1 else default_socket()
    nonce = os.urandom(4).hex()
    name = f"aira-324-probe-{nonce}"
    base = {
        "slice": "aira.slice", "reserve": 1 << 20, "pinned": True, "discountable": True,
        "scope_id": scope_id(name), "name": name, "owner": "unknown", "max_wait_ms": 0,
    }
    largest = dict(base, cpu=1, signature=f"aira-324-probe {nonce}", priority="high",
                   exclusive_holder=scope_id(f"{name}-holder"))
    largest["scope_id"] = scope_id(f"{name}-b")
    failures = 0
    for label, args in (("minimal (8 fields)", base), ("largest (13 fields)", largest)):
        reply = exchange(sock, args)
        verdict = "as expected" if (not reply.get("ok") and reply.get("code") == "E_DAEMON_PROTOCOL" and reply.get("error") == EXPECTED) else "UNEXPECTED"
        failures += verdict != "as expected"
        print(f"{label}: ok={reply.get('ok')} code={reply.get('code')} error={reply.get('error')!r} -> {verdict}")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
