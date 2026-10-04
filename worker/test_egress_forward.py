"""One client request opens one relay splice. The next request opens another."""
import importlib.util
import pathlib
import socket
import threading
import time

spec = importlib.util.spec_from_file_location(
    "egress_forward", pathlib.Path(__file__).with_name("egress-forward.py")
)
ef = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ef)


def test_relay_prelude_is_per_request():
    hits = {"n": 0}
    srv = socket.socket()
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", 0))
    port = srv.getsockname()[1]
    srv.listen(8)
    srv.settimeout(3)

    def serve():
        while hits["n"] < 2:
            try:
                c, _ = srv.accept()
            except socket.timeout:
                return
            data = b""
            c.settimeout(1)
            try:
                while b"\r\n\r\n" not in data and len(data) < 4096:
                    chunk = c.recv(4096)
                    if not chunk:
                        break
                    data += chunk
            except socket.timeout:
                pass
            if data.startswith(b"POST /gw/abc/in "):
                hits["n"] += 1
                c.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            c.close()

    threading.Thread(target=serve, daemon=True).start()
    ef.UPSTREAM_RAW = "http://127.0.0.1:%s/gw/abc" % port
    ef.WORKER_TOKEN = "tok"
    ef.RETRY_SECS = 2
    ef._direct_upstream = ""
    ef.PUBLIC_URL_FILE = ""

    up = ef.dial_upstream(time.monotonic() + 2)
    up.close()
    up2 = ef.dial_upstream(time.monotonic() + 2)
    up2.close()
    time.sleep(0.2)
    srv.close()
    assert hits["n"] == 2, hits


if __name__ == "__main__":
    test_relay_prelude_is_per_request()
    print("ok")
