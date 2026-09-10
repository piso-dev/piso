#!/usr/bin/env python3
# Bind the worker's vpc IPv4:PORT and TCP-proxy to 127.0.0.1:PORT so the
# gateway (which dials the container address) can reach a loopback-only
# listener — Plannotator on 19432, later OAuth callbacks, etc.
#
# Binding 0.0.0.0:PORT would EADDRINUSE against the loopback socket; binding
# the vpc address does not. No iptables (CAP_NET_ADMIN is dropped).
#
# Usage: piso-loopback-forward PORT
# Discovers the vpc IP by UDP-connecting to GATEWAY_URL (no iproute2).
from __future__ import print_function

import os
import select
import signal
import socket
import sys
import threading

try:
    from urllib.parse import urlparse
except ImportError:
    from urlparse import urlparse  # type: ignore


def vpc_ip():
    raw = os.environ.get("GATEWAY_URL") or "http://gateway:8083"
    parsed = urlparse(raw)
    host = parsed.hostname or "gateway"
    port = parsed.port or 8083
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.connect((host, port))
        ip = s.getsockname()[0]
    finally:
        s.close()
    return ip


def pipe(a, b):
    try:
        while True:
            readable, _, _ = select.select([a, b], [], [], 120)
            if not readable:
                continue
            for src, dst in ((a, b), (b, a)):
                if src not in readable:
                    continue
                data = src.recv(65536)
                if not data:
                    return
                dst.sendall(data)
    except Exception:
        return
    finally:
        for sock in (a, b):
            try:
                sock.close()
            except Exception:
                pass


def handle(client, dest_port):
    up = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        up.settimeout(5)
        up.connect(("127.0.0.1", dest_port))
        up.settimeout(None)
        pipe(client, up)
    except Exception:
        try:
            client.close()
        except Exception:
            pass
        try:
            up.close()
        except Exception:
            pass


def main():
    if len(sys.argv) != 2:
        print("usage: piso-loopback-forward PORT", file=sys.stderr)
        sys.exit(2)
    try:
        port = int(sys.argv[1])
    except ValueError:
        print("piso-loopback-forward: port must be an integer", file=sys.stderr)
        sys.exit(2)
    if port < 1 or port > 65535:
        print("piso-loopback-forward: port out of range", file=sys.stderr)
        sys.exit(2)

    host = vpc_ip()
    if not host or host.startswith("127.") or host == "0.0.0.0":
        print("piso-loopback-forward: could not discover vpc ip (got %r)" % (host,), file=sys.stderr)
        sys.exit(1)

    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        srv.bind((host, port))
    except OSError as e:
        print("piso-loopback-forward: bind %s:%d: %s" % (host, port, e), file=sys.stderr)
        sys.exit(1)
    srv.listen(32)
    print("piso-loopback-forward: %s:%d -> 127.0.0.1:%d" % (host, port, port), flush=True)

    def _die(signum, frame):
        sys.exit(0)

    signal.signal(signal.SIGTERM, _die)
    signal.signal(signal.SIGINT, _die)

    while True:
        try:
            client, _addr = srv.accept()
        except Exception:
            break
        t = threading.Thread(target=handle, args=(client, port))
        t.daemon = True
        t.start()


if __name__ == "__main__":
    main()
