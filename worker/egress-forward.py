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
import ssl
import sys
import threading
import time
from urllib.parse import urlparse


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


UPSTREAM_RAW = (os.environ.get("PISO_EGRESS_UPSTREAM") or "gateway:8080").strip()
LISTEN_HOST, LISTEN_PORT = parse_hostport(os.environ.get("PISO_EGRESS_LISTEN") or "127.0.0.1:18080", 18080)
RETRY_SECS = env_int("PISO_EGRESS_RETRY_SECS", 45)
WORKER_TOKEN = (os.environ.get("PISO_WORKER_TOKEN") or "").strip()
PUBLIC_URL_FILE = (os.environ.get("PISO_PUBLIC_URL_FILE") or "").strip()
# direct_upstream is the gateway public URL learned from check-in. Empty
# means keep using the launch assignment (relay or vpc gateway).
_upstream_lock = threading.Lock()
_direct_upstream = ""
_skip_direct = False


def parse_upstream(raw):
    if "://" in raw:
        u = urlparse(raw)
        host = u.hostname or "gateway"
        port = u.port or (443 if u.scheme == "https" else 80)
        path = u.path or ""
        return u.scheme, host, port, path.rstrip("/")
    host, port = parse_hostport(raw, 8080)
    return "http", host, port, ""


def current_upstream():
    with _upstream_lock:
        if _skip_direct:
            raw = UPSTREAM_RAW
        else:
            raw = _direct_upstream or UPSTREAM_RAW
    return parse_upstream(raw)


def set_direct_upstream(url):
    """Switch later dials to the gateway public URL. Relay stays available until a direct check-in succeeds."""
    global _direct_upstream
    url = (url or "").strip().rstrip("/")
    if not url:
        return False
    with _upstream_lock:
        if _direct_upstream == url:
            return False
        _direct_upstream = url
    print("piso-egress-forward: direct upstream %s" % url, flush=True)
    return True


def using_direct():
    with _upstream_lock:
        return bool(_direct_upstream)


def refresh_direct_upstream():
    if not PUBLIC_URL_FILE:
        return
    try:
        raw = open(PUBLIC_URL_FILE, "r").read()
    except OSError:
        return
    url = raw.strip().splitlines()[0].strip() if raw.strip() else ""
    if url:
        set_direct_upstream(url)


UP_SCHEME, UPSTREAM_HOST, UPSTREAM_PORT, UP_PATH = parse_upstream(UPSTREAM_RAW)


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


def _ssl_wrap(sock, server_hostname):
    ctx = ssl.create_default_context()
    ca = os.environ.get("SSL_CERT_FILE") or os.environ.get("CURL_CA_BUNDLE")
    if ca and os.path.isfile(ca):
        try:
            ctx.load_verify_locations(cafile=ca)
        except Exception:
            pass
    return ctx.wrap_socket(sock, server_hostname=server_hostname)


def dial_upstream(deadline):
    """Open one gateway session for one client request.

    A relay assignment (path /gw/<id>) sends POST /in and expects 200 before
    the client request is written. That splice is single-use: the caller must
    dial again for the next request. A direct public URL has no prelude.
    A failed direct dial falls back to the launch assignment for this call
    only; the next call tries the public URL again.
    """
    global _skip_direct
    delay = 0.2
    announced = False
    last = None
    with _upstream_lock:
        _skip_direct = False
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise OSError(last or "egress dial timed out")
        refresh_direct_upstream()
        scheme, host, port, path = current_upstream()
        try:
            raw = socket.create_connection((host, port), timeout=min(5.0, remaining))
            raw.settimeout(None)
            if scheme == "https":
                up = _ssl_wrap(raw, host)
            else:
                up = raw
            if path.startswith("/gw/"):
                prelude = (
                    "POST %s/in HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n"
                    % (path, host)
                )
                if WORKER_TOKEN:
                    prelude += "X-Piso-Worker-Token: %s\r\n" % WORKER_TOKEN
                prelude += "\r\n"
                send_all(up, prelude.encode("ascii"))
                head, extra = read_headers(up)
                if b" 200 " not in head.split(b"\r\n", 1)[0]:
                    try:
                        up.close()
                    except Exception:
                        pass
                    raise OSError("relay prelude failed: %s" % head.split(b"\r\n", 1)[0])
            return up
        except OSError as exc:
            # A learned public URL that is not reachable yet must not strand
            # the worker. Drop it for this attempt and use the launch relay.
            if using_direct() and not path.startswith("/gw/") and not _skip_direct:
                print(
                    "piso-egress-forward: direct dial failed (%s); falling back to launch assignment"
                    % exc,
                    file=sys.stderr,
                    flush=True,
                )
                with _upstream_lock:
                    _skip_direct = True
                announced = False
                continue
            last = exc
            if not announced:
                print(
                    "piso-egress-forward: dial %s://%s:%s%s failed (%s); retrying up to %ss"
                    % (scheme, host, port, path, exc, RETRY_SECS),
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
        if WORKER_TOKEN and b"\r\n\r\n" in head and b"X-Piso-Worker-Token:" not in head and b"Proxy-Authorization:" not in head:
            insert = ("Proxy-Authorization: Bearer %s\r\nX-Piso-Worker-Token: %s\r\n" % (WORKER_TOKEN, WORKER_TOKEN)).encode("ascii")
            head = head[:-2] + insert + b"\r\n"
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
        "piso-egress-forward: %s:%s -> %s://%s:%s%s (dial retry %ss)"
        % (LISTEN_HOST, LISTEN_PORT, UP_SCHEME, UPSTREAM_HOST, UPSTREAM_PORT, UP_PATH, RETRY_SECS),
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
