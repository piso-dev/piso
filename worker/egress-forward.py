#!/usr/bin/env python3
# Local HTTP proxy in front of the gateway MITM.
#
# Workers set HTTP(S)_PROXY / ALL_PROXY here (127.0.0.1:18080). This process
# dials PISO_EGRESS_UPSTREAM (default gateway:8080) and retries only while
# that dial fails — connection refused, reset, timeout, DNS — for about 45s.
# That covers a gateway container recreate. Once the TCP connection is up,
# the buffered client head is written once and the sockets are spliced.
# A reset after that write is not replayed, so a 407 or a POST the gateway
# may already have seen is never sent twice.
#
# Raw sockets: this process does not honor HTTP_PROXY, so it cannot loop
# back into itself. NO_PROXY still keeps gateway:8083 (worker API) direct.
from __future__ import print_function

import os
import select
import socket
import sys
import threading
import time


def env_int(name, default):
    raw = os.environ.get(name, "")
    try:
        return int(raw) if raw else default
    except ValueError:
        return default


def parse_hostport(raw, default_port):
    raw = (raw or "").strip()
    if not raw:
        return "gateway", default_port
    host, sep, port_s = raw.rpartition(":")
    if not sep or not host:
        return raw, default_port
    try:
        return host, int(port_s)
    except ValueError:
        return raw, default_port


UPSTREAM_HOST, UPSTREAM_PORT = parse_hostport(os.environ.get("PISO_EGRESS_UPSTREAM"), 8080)
LISTEN_HOST, LISTEN_PORT = parse_hostport(os.environ.get("PISO_EGRESS_LISTEN") or "127.0.0.1:18080", 18080)
RETRY_SECS = env_int("PISO_EGRESS_RETRY_SECS", 45)


def read_headers(sock, limit=65536):
    buf = b""
    while b"\r\n\r\n" not in buf and len(buf) < limit:
        chunk = sock.recv(4096)
        if not chunk:
            break
        buf += chunk
    idx = buf.find(b"\r\n\r\n")
    if idx < 0:
        return buf, b""
    end = idx + 4
    return buf[:end], buf[end:]


def dial_upstream(deadline):
    delay = 0.2
    announced = False
    last = None
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise OSError(last or "egress dial timed out")
        try:
            up = socket.create_connection((UPSTREAM_HOST, UPSTREAM_PORT), timeout=min(5.0, remaining))
            up.settimeout(None)
            return up
        except OSError as exc:
            last = exc
            if not announced:
                print(
                    "piso-egress-forward: dial %s:%s failed (%s); retrying up to %ss"
                    % (UPSTREAM_HOST, UPSTREAM_PORT, exc, RETRY_SECS),
                    file=sys.stderr,
                    flush=True,
                )
                announced = True
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise
            time.sleep(min(delay, remaining))
            delay = min(delay * 2, 5.0)


def send_all(sock, data):
    view = memoryview(data)
    while view:
        n = sock.send(view)
        if n <= 0:
            raise OSError("short send")
        view = view[n:]


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
                send_all(dst, data)
    except Exception:
        return
    finally:
        for sock in (a, b):
            try:
                sock.close()
            except Exception:
                pass


def reject(client, status, reason):
    try:
        send_all(client, ("HTTP/1.1 %s %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n" % (status, reason)).encode("ascii"))
    except Exception:
        pass
    try:
        client.close()
    except Exception:
        pass


def handle(client):
    up = None
    try:
        client.settimeout(10)
        head, extra = read_headers(client)
        client.settimeout(None)
        if b"\r\n\r\n" not in head:
            reject(client, 400, "Bad Request")
            return
        deadline = time.monotonic() + RETRY_SECS
        try:
            up = dial_upstream(deadline)
        except OSError as exc:
            print("piso-egress-forward: gave up dialing gateway: %s" % exc, file=sys.stderr, flush=True)
            reject(client, 502, "Bad Gateway")
            return
        # Gateway has accepted the TCP connection. Write once. Do not replay.
        send_all(up, head)
        if extra:
            send_all(up, extra)
        pipe(client, up)
    except Exception:
        try:
            client.close()
        except Exception:
            pass
        if up is not None:
            try:
                up.close()
            except Exception:
                pass


def main():
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((LISTEN_HOST, LISTEN_PORT))
    srv.listen(128)
    print(
        "piso-egress-forward: %s:%s -> %s:%s (dial retry %ss)"
        % (LISTEN_HOST, LISTEN_PORT, UPSTREAM_HOST, UPSTREAM_PORT, RETRY_SECS),
        flush=True,
    )
    while True:
        client, _ = srv.accept()
        threading.Thread(target=handle, args=(client,), daemon=True).start()


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(0)
    except Exception as exc:
        print("piso-egress-forward: %s" % exc, file=sys.stderr, flush=True)
        sys.exit(1)
