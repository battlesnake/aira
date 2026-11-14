#!/usr/bin/env python3
"""AIRA-323: what a daemon that predates the four completion facts answers.

Sends two raw confine-report frames, each with a fresh nonce signature, to the
live daemon socket and prints the answers, then checks (read-only) that neither
nonce reached the store:

  four fields  {signature, oom, wall_ms, started_at_ms}  -> the allowlist path
  nine fields  {signature, oom, peak_rss, budget, budget_basis,
                wall_ms, wait_ms, started_at_ms, repo}   -> the count-guard path

Against a pre-323 daemon both must be refused with E_DAEMON_PROTOCOL and write
nothing (the client's once-only re-send relies on this). Against a 323 daemon
both are accepted and two rows are written (signatures aira-323-probe-*).

Usage: python3 docs/dev/aira-323-old-daemon-probe.py [socket] [state.db]
"""
import json
import os
import pathlib
import socket
import sqlite3
import struct
import sys
import time


def default_socket() -> str:
    runtime = pathlib.Path(os.environ.get("XDG_RUNTIME_DIR", f"/run/user/{os.getuid()}")) / "aira"
    sockets = sorted(runtime.glob("*/daemon.sock"))
    if len(sockets) != 1:
        sys.exit(f"cannot choose a daemon socket under {runtime}: {sockets}")
    return str(sockets[0])


def exchange(path: str, args: dict) -> dict:
    frame = {"proto": 15, "scope": {}, "request": {"verb": "confine-report", "args": args, "has_content": False}}
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


def main() -> int:
    sock = sys.argv[1] if len(sys.argv) > 1 else default_socket()
    state = os.environ.get("XDG_STATE_HOME", str(pathlib.Path.home() / ".local/state"))
    db = sys.argv[2] if len(sys.argv) > 2 else str(pathlib.Path(state) / "aira/state.db")
    now = int(time.time() * 1000)
    nonce = os.urandom(8).hex()
    cases = {
        "four fields (allowlist path)": {
            "signature": f"aira-323-probe-{nonce}-a", "oom": False, "wall_ms": 1000, "started_at_ms": now,
        },
        "nine fields (count-guard path)": {
            "signature": f"aira-323-probe-{nonce}-b", "oom": False, "peak_rss": 1048576,
            "budget": 33554432, "budget_basis": "cap:operator:--memory-reserve",
            "wall_ms": 1000, "wait_ms": 0, "started_at_ms": now, "repo": "none",
        },
    }
    for name, args in cases.items():
        reply = exchange(sock, args)
        print(f"{name}: ok={reply.get('ok')} code={reply.get('code')} error={reply.get('error')!r}")
    conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    (rows,) = conn.execute(
        "select count(*) from confine_peak_history where signature like ?", (f"aira-323-probe-{nonce}-%",)
    ).fetchone()
    print(f"rows written for this probe's nonce: {rows}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
