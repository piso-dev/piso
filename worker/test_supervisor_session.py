import importlib.util
import json
import os
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

spec = importlib.util.spec_from_file_location(
    "supervisor_session", Path(__file__).with_name("supervisor-session.py")
)
session = importlib.util.module_from_spec(spec)
spec.loader.exec_module(session)

FAKE_PI = """#!/usr/bin/env python3
import json, sys
while True:
    line = sys.stdin.readline()
    if not line:
        break
    try:
        cmd = json.loads(line)
    except ValueError:
        continue
    kind = cmd.get("type")
    if kind == "prompt":
        text = cmd.get("message", "")
        sys.stdout.write(json.dumps({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": "echo: " + text}}) + "\\n")
        sys.stdout.write(json.dumps({"type": "agent_settled"}) + "\\n")
        sys.stdout.flush()
    elif kind == "steer":
        sys.stdout.write(json.dumps({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": "steered"}}) + "\\n")
        sys.stdout.write(json.dumps({"type": "agent_settled"}) + "\\n")
        sys.stdout.flush()
"""


class FakeGateway:
    def __init__(self):
        self.worker = "piso-worker-ship"
        self.slug = "ship"
        self.replies = []
        self.acked = []
        self.leases = 0

    def lease(self):
        self.leases += 1
        return {"granted": True}

    def messages(self, wait):
        return {"messages": []}

    def ack(self, ids):
        self.acked.extend(ids)

    def reply(self, chat_id, text):
        self.replies.append((chat_id, text))


class FakePi:
    def __init__(self, on_event=None):
        self.on_event = on_event or (lambda _r: None)
        self.sent = []
        self.busy = False

    def send(self, obj):
        self.sent.append(obj)

    def alive(self):
        return True

    def stop(self):
        pass


class SupervisorLogicTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.addCleanup(self.temp.cleanup)
        session.STATE_DIR = self.temp.name
        session.STATE_FILE = os.path.join(self.temp.name, "supervisor.json")
        session.STATE_PLAIN = os.path.join(self.temp.name, "supervisor.state")

    def make(self):
        gw = FakeGateway()
        sup = session.Supervisor(gw, lambda: FakePi())
        sup.pi = FakePi(sup.on_event)
        return gw, sup

    def test_text_deltas_become_one_reply_per_turn(self):
        gw, sup = self.make()
        sup.feed({"chatId": 9, "text": "hi"})
        sup.on_event({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": "Hel"}})
        sup.on_event({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": "lo"}})
        # Nothing is posted until the turn settles.
        self.assertEqual(gw.replies, [])
        sup.on_event({"type": "agent_settled"})
        self.assertEqual(gw.replies, [(9, "Hello")])
        # The buffer does not leak into the next turn.
        sup.on_event({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": "x"}})
        self.assertEqual(gw.replies, [(9, "Hello")])

    def test_idle_message_is_a_prompt_and_busy_message_steers(self):
        _gw, sup = self.make()
        sup.feed({"chatId": 1, "text": "first"})
        self.assertEqual(sup.pi.sent[-1], {"type": "prompt", "message": "first"})
        self.assertTrue(sup.pi.busy)
        sup.feed({"chatId": 1, "text": "second"})
        self.assertEqual(sup.pi.sent[-1], {"type": "steer", "message": "second"})

    def test_control_words_drive_the_session(self):
        _gw, sup = self.make()
        sup.pi.busy = True
        sup.feed({"chatId": 1, "text": "/interrupt"})
        self.assertEqual(sup.pi.sent[-1], {"type": "abort"})
        self.assertTrue(sup.pi.busy, "an abort must not mark the session idle by itself")
        sup.feed({"chatId": 1, "text": "/new"})
        self.assertEqual(sup.pi.sent[-1], {"type": "new_session"})

    def test_lease_state_is_written(self):
        _gw, sup = self.make()
        sup._set_supervisor(True)
        self.assertTrue(sup.is_supervisor())
        with open(session.STATE_PLAIN) as fh:
            self.assertEqual(fh.read().strip(), "supervisor")
        sup._set_supervisor(False, "supervisor_taken")
        with open(session.STATE_PLAIN) as fh:
            self.assertEqual(fh.read().strip(), "not-supervisor:supervisor_taken")

    def test_drain_acks_every_message(self):
        gw, sup = self.make()
        gw.messages = lambda wait: {"messages": [
            {"id": "m1", "chatId": 9, "text": "one"},
            {"id": "m2", "chatId": 9, "text": "two"},
        ]}
        sup.pi = FakePi(sup.on_event)
        count = sup.drain_once()
        self.assertEqual(count, 2)
        self.assertEqual(gw.acked, ["m1", "m2"])
        self.assertEqual(sup.pi.sent[-1], {"type": "steer", "message": "two"})


class PiRpcTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.addCleanup(self.temp.cleanup)
        self.script = Path(self.temp.name) / "fake_pi.py"
        self.script.write_text(FAKE_PI)

    def test_rpc_prompt_streams_text_and_settles(self):
        events = []
        done = threading.Event()

        def on_event(rec):
            events.append(rec)
            if rec.get("type") == "agent_settled":
                done.set()

        rpc = session.PiRpc([sys.executable, str(self.script)], on_event)
        self.addCleanup(rpc.stop)
        rpc.send({"type": "prompt", "message": "hello"})
        self.assertTrue(done.wait(5), "fake pi did not settle")
        deltas = []
        for rec in events:
            if rec.get("type") == "message_update":
                deltas.append(rec["assistantMessageEvent"].get("delta", ""))
        self.assertEqual("".join(deltas), "echo: hello")


class GatewayClientTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.addCleanup(self.temp.cleanup)
        session.STATE_DIR = self.temp.name
        session.STATE_FILE = os.path.join(self.temp.name, "supervisor.json")
        session.STATE_PLAIN = os.path.join(self.temp.name, "supervisor.state")
        self.state = {"messages": [{"id": "m1", "chatId": 9, "text": "hi"}],
                      "replies": [], "acked": [], "not_supervisor": False}
        state = self.state

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _json(self, code, body):
                raw = json.dumps(body).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                body = json.loads(self.rfile.read(length) or b"{}")
                if self.path == "/api/v1/worker/supervisor":
                    self._json(200, {"granted": True})
                elif self.path == "/api/v1/worker/messages/ack":
                    state["acked"].extend(body.get("ids", []))
                    self._json(200, {"ok": True})
                elif self.path == "/api/v1/worker/reply":
                    state["replies"].append((body.get("chatId"), body.get("text")))
                    self._json(200, {"ok": True})
                else:
                    self._json(404, {"error": "not found"})

            def do_GET(self):
                if state["not_supervisor"]:
                    self._json(409, {"error": "not the supervisor"})
                    return
                if self.path.startswith("/api/v1/worker/messages"):
                    pending, state["messages"] = state["messages"], []
                    self._json(200, {"messages": pending})
                else:
                    self._json(404, {"error": "not found"})

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.addCleanup(self.server.shutdown)
        self.addCleanup(self.server.server_close)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.base = "http://127.0.0.1:%d" % self.server.server_address[1]

    def test_client_round_trip(self):
        gw = session.Gateway(self.base, "w", "s")
        self.assertTrue(gw.lease()["granted"])
        result = gw.messages(1)
        self.assertEqual(result["messages"][0]["id"], "m1")
        gw.ack(["m1"])
        gw.reply(9, "answer")
        self.assertEqual(self.state["acked"], ["m1"])
        self.assertEqual(self.state["replies"], [(9, "answer")])
        # The mailbox is empty on the next poll.
        self.assertEqual(gw.messages(1)["messages"], [])

    def test_conflict_raises_not_supervisor(self):
        self.state["not_supervisor"] = True
        gw = session.Gateway(self.base, "w", "s")
        with self.assertRaises(session.NotSupervisor):
            gw.messages(1)


class EndToEndTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.addCleanup(self.temp.cleanup)
        session.STATE_DIR = self.temp.name
        session.STATE_FILE = os.path.join(self.temp.name, "supervisor.json")
        session.STATE_PLAIN = os.path.join(self.temp.name, "supervisor.state")
        self.script = Path(self.temp.name) / "fake_pi.py"
        self.script.write_text(FAKE_PI)
        self.state = {"messages": [{"id": "m1", "chatId": 9, "text": "ping"}],
                      "replies": []}
        state = self.state

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _json(self, code, body):
                raw = json.dumps(body).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                body = json.loads(self.rfile.read(length) or b"{}")
                if self.path == "/api/v1/worker/supervisor":
                    self._json(200, {"granted": True})
                elif self.path == "/api/v1/worker/reply":
                    state["replies"].append((body.get("chatId"), body.get("text")))
                    self._json(200, {"ok": True})
                else:
                    self._json(200, {"ok": True})

            def do_GET(self):
                pending, state["messages"] = state["messages"], []
                self._json(200, {"messages": pending})

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.addCleanup(self.server.shutdown)
        self.addCleanup(self.server.server_close)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.base = "http://127.0.0.1:%d" % self.server.server_address[1]

    def test_message_reaches_pi_and_reply_returns(self):
        gw = session.Gateway(self.base, "w", "s")
        sup = session.Supervisor(
            gw, lambda: session.PiRpc([sys.executable, str(self.script)], lambda _r: None),
            wait=1,
        )
        thread = threading.Thread(target=sup.run, daemon=True)
        thread.start()
        deadline = time.time() + 5
        while time.time() < deadline and not self.state["replies"]:
            time.sleep(0.05)
        sup.stop()
        thread.join(timeout=3)
        self.assertEqual(self.state["replies"], [(9, "echo: ping")])


if __name__ == "__main__":
    unittest.main()
