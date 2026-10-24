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

# A stand-in for `pi --mode rpc`: it echoes every command id on its response the
# way the real thing does, and reports one tool call so the activity line has
# something to show.
FAKE_PI = """#!/usr/bin/env python3
import json, sys

def emit(rec):
    sys.stdout.write(json.dumps(rec) + "\\n")
    sys.stdout.flush()

while True:
    line = sys.stdin.readline()
    if not line:
        break
    try:
        cmd = json.loads(line)
    except ValueError:
        continue
    kind = cmd.get("type")
    cid = cmd.get("id")
    if kind == "prompt":
        emit({"id": cid, "type": "response", "command": "prompt", "success": True,
              "data": {"disposition": "started"}})
        emit({"type": "tool_execution_start", "toolCallId": "t1", "toolName": "bash",
              "args": {"command": "echo hi"}})
        emit({"type": "tool_execution_end", "toolCallId": "t1", "toolName": "bash",
              "result": {"content": [{"type": "text", "text": "hi"}]}, "isError": False})
        emit({"type": "message_update",
              "assistantMessageEvent": {"type": "text_delta", "delta": "echo: " + cmd.get("message", "")}})
        emit({"type": "agent_settled"})
    elif kind == "steer":
        emit({"id": cid, "type": "response", "command": "steer", "success": True,
              "data": {"disposition": "queued"}})
        emit({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": "steered"}})
        emit({"type": "agent_settled"})
    elif kind == "get_state":
        emit({"id": cid, "type": "response", "command": "get_state", "success": True,
              "data": {"model": {"id": "fake/model", "provider": "fake"}, "messageCount": 3}})
    elif kind == "new_session":
        emit({"id": cid, "type": "response", "command": "new_session", "success": True,
              "data": {"cancelled": False}})
    else:
        emit({"id": cid, "type": "response", "command": kind, "success": True})
"""


class FakeGateway:
    def __init__(self):
        self.worker = "piso-worker-ship"
        self.slug = "ship"
        self.replies = []
        self.acked = []
        self.leases = 0
        self.statuses = []
        self.status_supported = True
        self._next_status = 100

    def lease(self):
        self.leases += 1
        return {"granted": True}

    def messages(self, wait):
        return {"messages": []}

    def ack(self, ids):
        self.acked.extend(ids)

    def reply(self, chat_id, text):
        self.replies.append((chat_id, text))

    def status(self, chat_id, text, message_id=0):
        if not self.status_supported:
            return None
        self.statuses.append((chat_id, text, message_id))
        if message_id:
            return message_id
        self._next_status += 1
        return self._next_status


class FakePi:
    """Records commands and lets a test answer one the way pi would."""

    def __init__(self, on_event=None, on_response=None):
        self.on_event = on_event or (lambda _r: None)
        self.on_response = on_response or (lambda _r, _h=False: None)
        self.sent = []
        self.handlers = {}
        self.alive_flag = True
        self.stopped = False

    def send(self, cmd, handler=None):
        command = dict(cmd)
        cid = command.get("id") or "c%d" % (len(self.sent) + 1)
        command["id"] = cid
        self.sent.append(command)
        if handler is not None:
            self.handlers[cid] = handler
        return cid

    def respond(self, cid, command, success=True, data=None, error=None):
        record = {"type": "response", "id": cid, "command": command, "success": success}
        if data is not None:
            record["data"] = data
        if error is not None:
            record["error"] = error
        handler = self.handlers.pop(cid, None)
        if handler is not None:
            handler(record)
        self.on_response(record, handler is not None)

    def alive(self):
        return self.alive_flag

    def stop(self):
        self.stopped = True


def last_sent(sup, chat_id):
    return sup.sessions[chat_id].rpc.sent[-1]


class SupervisorLogicTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.addCleanup(self.temp.cleanup)
        session.STATE_DIR = self.temp.name
        session.STATE_FILE = os.path.join(self.temp.name, "supervisor.json")
        session.STATE_PLAIN = os.path.join(self.temp.name, "supervisor.state")
        self.spawned = []

    def make(self):
        gw = FakeGateway()

        def spawn(chat_id, on_event, on_response):
            self.spawned.append(chat_id)
            return FakePi(on_event, on_response)

        return gw, session.Supervisor(gw, spawn)

    def test_text_deltas_become_one_reply_per_turn(self):
        gw, sup = self.make()
        sup.feed({"chatId": 9, "text": "hi"})
        sup.on_event(sup.sessions[9], {"type": "message_update",
                                       "assistantMessageEvent": {"type": "text_delta", "delta": "Hel"}})
        sup.on_event(sup.sessions[9], {"type": "message_update",
                                       "assistantMessageEvent": {"type": "text_delta", "delta": "lo"}})
        # Nothing is posted until the turn settles.
        self.assertEqual(gw.replies, [])
        sup.on_event(sup.sessions[9], {"type": "agent_settled"})
        self.assertEqual(gw.replies, [(9, "Hello")])
        # The buffer does not leak into the next turn.
        sup.on_event(sup.sessions[9], {"type": "message_update",
                                       "assistantMessageEvent": {"type": "text_delta", "delta": "x"}})
        self.assertEqual(gw.replies, [(9, "Hello")])

    def test_idle_message_is_a_prompt_and_busy_message_steers(self):
        _gw, sup = self.make()
        sup.feed({"chatId": 1, "text": "first"})
        self.assertEqual(last_sent(sup, 1)["type"], "prompt")
        self.assertEqual(last_sent(sup, 1)["message"], "first")
        self.assertTrue(sup.sessions[1].busy)
        sup.feed({"chatId": 1, "text": "second"})
        self.assertEqual(last_sent(sup, 1)["type"], "steer")
        self.assertEqual(last_sent(sup, 1)["message"], "second")

    def test_control_words_drive_the_session(self):
        gw, sup = self.make()
        sup.feed({"chatId": 1, "text": "/stop"})
        self.assertEqual(gw.replies[-1], (1, "Nothing is running."))
        sup.sessions[1].busy = True
        sup.feed({"chatId": 1, "text": "/interrupt"})
        self.assertEqual(last_sent(sup, 1)["type"], "abort")
        self.assertTrue(sup.sessions[1].busy, "an abort must not mark the session idle by itself")
        sup.feed({"chatId": 1, "text": "/new"})
        self.assertEqual(last_sent(sup, 1)["type"], "new_session")

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
        count = sup.drain_once()
        self.assertEqual(count, 2)
        self.assertEqual(gw.acked, ["m1", "m2"])
        self.assertEqual(last_sent(sup, 9)["message"], "two")

    # ---- one session per chat ----

    def test_each_chat_gets_its_own_pi_process(self):
        _gw, sup = self.make()
        sup.feed({"chatId": 1, "text": "hello from one"})
        sup.feed({"chatId": 2, "text": "hello from two"})
        self.assertEqual(self.spawned, [1, 2])
        self.assertEqual(last_sent(sup, 1)["message"], "hello from one")
        self.assertEqual(last_sent(sup, 2)["message"], "hello from two")
        # A turn on one chat does not make the other one busy.
        self.assertTrue(sup.sessions[1].busy)
        self.assertTrue(sup.sessions[2].busy)
        sup.on_event(sup.sessions[1], {"type": "agent_settled"})
        self.assertFalse(sup.sessions[1].busy)
        self.assertTrue(sup.sessions[2].busy)

    def test_session_ids_are_stable_and_argv_carries_one(self):
        self.assertEqual(session.session_id(9), "tg-9")
        self.assertEqual(session.session_id(9), session.session_id(9))
        self.assertEqual(session.session_id(-1001), "tg-m1001")
        argv = session.pi_argv(9)
        self.assertIn("--session-id", argv)
        self.assertEqual(argv[argv.index("--session-id") + 1], "tg-9")

    def test_idle_sessions_are_evicted_but_busy_ones_survive(self):
        _gw, sup = self.make()
        old_cap = session.MAX_SESSIONS
        session.MAX_SESSIONS = 2
        self.addCleanup(setattr, session, "MAX_SESSIONS", old_cap)
        sup.feed({"chatId": 1, "text": "a"})
        sup.on_event(sup.sessions[1], {"type": "agent_settled"})  # chat 1 idle
        sup.feed({"chatId": 2, "text": "b"})
        sup.feed({"chatId": 3, "text": "c"})  # chat 1 is the LRU idle one
        self.assertNotIn(1, sup.sessions)
        self.assertIn(2, sup.sessions)
        self.assertIn(3, sup.sessions)

    # ---- correlated commands ----

    def test_rejected_prompt_is_reported_and_clears_busy(self):
        gw, sup = self.make()
        sup.feed({"chatId": 5, "text": "hi"})
        cid = last_sent(sup, 5)["id"]
        sup.sessions[5].rpc.respond(cid, "prompt", success=False, error="cannot prompt while streaming")
        self.assertFalse(sup.sessions[5].busy)
        self.assertEqual(len(gw.replies), 1)
        self.assertIn("⚠️ prompt", gw.replies[0][1])
        self.assertIn("cannot prompt while streaming", gw.replies[0][1])

    def test_handled_disposition_leaves_the_session_idle(self):
        _gw, sup = self.make()
        sup.feed({"chatId": 5, "text": "/some-extension-command"})
        cid = last_sent(sup, 5)["id"]
        sup.sessions[5].rpc.respond(cid, "prompt", data={"disposition": "handled"})
        self.assertFalse(sup.sessions[5].busy)

    def test_model_error_is_reported(self):
        gw, sup = self.make()
        sup.feed({"chatId": 5, "text": "hi"})
        sup.on_event(sup.sessions[5], {"type": "message_end",
                                       "message": {"role": "assistant", "stopReason": "error",
                                                   "errorMessage": "529 overloaded"}})
        self.assertEqual(gw.replies[-1], (5, "⚠️ 529 overloaded"))

    # ---- the activity line ----

    def test_tool_activity_is_one_edited_status_line(self):
        gw, sup = self.make()
        sup.feed({"chatId": 7, "text": "run it"})
        sess = sup.sessions[7]
        sup.on_event(sess, {"type": "tool_execution_start", "toolCallId": "t1",
                            "toolName": "bash", "args": {"command": "go test ./..."}})
        self.assertEqual(len(gw.statuses), 1)
        chat, text, message_id = gw.statuses[0]
        self.assertEqual(chat, 7)
        self.assertEqual(message_id, 0, "the first line is a new message")
        self.assertIn("bash", text)
        self.assertIn("go test", text)
        sup.on_event(sess, {"type": "tool_execution_end", "toolCallId": "t1",
                            "toolName": "bash", "isError": False, "durationMs": 1200})
        # The second write edits the same message instead of posting another.
        self.assertEqual(len(gw.statuses), 2)
        self.assertEqual(gw.statuses[1][2], gw._next_status)
        self.assertIn("✅ bash", gw.statuses[1][1])
        self.assertIn("1.2s", gw.statuses[1][1])
        self.assertEqual(sess.status_id, gw._next_status)

    def test_settle_writes_the_summary_then_starts_a_new_line(self):
        gw, sup = self.make()
        sup.feed({"chatId": 7, "text": "run it"})
        sess = sup.sessions[7]
        sup.on_event(sess, {"type": "tool_execution_start", "toolCallId": "t1",
                            "toolName": "bash", "args": {"command": "ls"}})
        sup.on_event(sess, {"type": "agent_settled"})
        self.assertTrue(gw.statuses[-1][1].startswith("✅ done"))
        self.assertIn("1 tool ·", gw.statuses[-1][1])
        self.assertEqual(sess.status_id, 0, "the next turn opens a fresh line")

    def test_aborted_turn_says_so(self):
        gw, sup = self.make()
        sup.feed({"chatId": 7, "text": "run it"})
        sess = sup.sessions[7]
        sup.on_event(sess, {"type": "tool_execution_start", "toolCallId": "t1",
                            "toolName": "bash", "args": {"command": "sleep 100"}})
        sup.on_event(sess, {"type": "agent_settled", "aborted": True})
        self.assertTrue(gw.statuses[-1][1].startswith("⏹ aborted"))

    def test_a_chat_that_only_talks_shows_no_status_line(self):
        gw, sup = self.make()
        sup.feed({"chatId": 7, "text": "hi"})
        sup.on_event(sup.sessions[7], {"type": "message_update",
                                       "assistantMessageEvent": {"type": "text_delta", "delta": "Hello"}})
        sup.on_event(sup.sessions[7], {"type": "agent_settled"})
        self.assertEqual(gw.statuses, [])
        self.assertEqual(gw.replies, [(7, "Hello")])

    def test_old_gateway_switches_activity_lines_off(self):
        gw, sup = self.make()
        gw.status_supported = False
        sup.feed({"chatId": 7, "text": "run it"})
        sess = sup.sessions[7]
        sup.on_event(sess, {"type": "tool_execution_start", "toolCallId": "t1",
                            "toolName": "bash", "args": {"command": "ls"}})
        self.assertFalse(sup.status_ok)
        sup.on_event(sess, {"type": "agent_settled"})
        self.assertEqual(gw.replies, [], "no status line is never an error in the chat")

    # ---- replies ----

    def test_long_replies_are_split_not_truncated(self):
        gw, sup = self.make()
        sup.feed({"chatId": 3, "text": "write a lot"})
        sess = sup.sessions[3]
        for _ in range(10):
            sup.on_event(sess, {"type": "message_update", "assistantMessageEvent":
                                {"type": "text_delta", "delta": "x" * 1000 + "\n"}})
        sup.on_event(sess, {"type": "agent_settled"})
        self.assertGreater(len(gw.replies), 1)
        for _chat, text in gw.replies:
            self.assertLessEqual(len(text), session.CHUNK_MAX)
        self.assertTrue("".join(t for _c, t in gw.replies).replace("\n", "").startswith("x" * 100))
        self.assertIn("x" * 100, gw.replies[-1][1], "the tail survives")

    def test_history_command_renders_the_transcript(self):
        gw, sup = self.make()
        sup.feed({"chatId": 4, "text": "/history"})
        cid = last_sent(sup, 4)["id"]
        self.assertEqual(last_sent(sup, 4)["type"], "get_messages")
        sup.sessions[4].rpc.respond(cid, "get_messages", data={"messages": [
            {"role": "user", "content": "what changed?"},
            {"role": "assistant", "content": [{"type": "text", "text": "two files"}]},
            {"role": "toolResult", "content": [{"type": "text", "text": "ignored"}]},
        ]})
        self.assertEqual(gw.replies[-1], (4, "🧑 what changed?\n\n🤖 two files"))

    def test_status_command_renders_the_model(self):
        gw, sup = self.make()
        sup.feed({"chatId": 4, "text": "/status"})
        cid = last_sent(sup, 4)["id"]
        sup.sessions[4].rpc.respond(cid, "get_state", data={
            "model": {"id": "fake/model", "provider": "fake"},
            "thinkingLevel": "medium", "messageCount": 7, "isStreaming": False,
        })
        text = gw.replies[-1][1]
        self.assertIn("fake/model", text)
        self.assertIn("thinking medium", text)
        self.assertIn("7 messages", text)
        self.assertIn("idle", text)

    def test_failed_control_command_is_reported(self):
        gw, sup = self.make()
        sup.feed({"chatId": 4, "text": "/status"})
        cid = last_sent(sup, 4)["id"]
        sup.sessions[4].rpc.respond(cid, "get_state", success=False, error="session not ready")
        self.assertEqual(gw.replies[-1], (4, "⚠️ status: session not ready"))

    def test_a_dead_session_is_restarted_with_its_own_id(self):
        gw, sup = self.make()
        sup.feed({"chatId": 8, "text": "hi"})
        sup.sessions[8].rpc.alive_flag = False
        sup.sessions[8].rpc.on_event({"type": "message_update",
                                      "assistantMessageEvent": {"type": "text_delta", "delta": "done"}})
        gw.messages = lambda wait: {"messages": []}
        sup.drain_once()
        self.assertEqual(self.spawned, [8, 8], "the same chat is restarted")
        self.assertEqual(gw.replies, [(8, "done")], "an answer that finished as it died still arrives")

    def test_a_restart_closes_the_frozen_activity_line(self):
        gw, sup = self.make()
        sup.feed({"chatId": 8, "text": "hi"})
        sess = sup.sessions[8]
        sup.tool_started(sess, {"toolCallId": "t9", "toolName": "bash",
                                "args": {"command": "sleep 90"}})
        sup.flush(sess, force=True)
        self.assertEqual(gw.statuses[-1][1], "🔧 bash sleep 90")
        sess.rpc.alive_flag = False
        gw.messages = lambda wait: {"messages": []}
        sup.drain_once()
        self.assertEqual(gw.statuses[-1][1], "♻️ pi restarted",
                         "the dead turn's line is closed out, not left hanging")


class PiRpcTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.addCleanup(self.temp.cleanup)
        self.script = Path(self.temp.name) / "fake_pi.py"
        self.script.write_text(FAKE_PI)

    def spawn(self, on_event):
        return session.PiRpc([sys.executable, str(self.script)], on_event)

    def test_rpc_prompt_streams_text_and_settles(self):
        events = []
        done = threading.Event()

        def on_event(rec):
            events.append(rec)
            if rec.get("type") == "agent_settled":
                done.set()

        rpc = self.spawn(on_event)
        self.addCleanup(rpc.stop)
        rpc.send({"type": "prompt", "message": "hello"})
        self.assertTrue(done.wait(5), "fake pi did not settle")
        deltas = []
        for rec in events:
            if rec.get("type") == "message_update":
                deltas.append(rec["assistantMessageEvent"].get("delta", ""))
        self.assertEqual("".join(deltas), "echo: hello")

    def test_every_command_carries_an_id_and_its_response_is_matched(self):
        rpc = self.spawn(lambda _r: None)
        self.addCleanup(rpc.stop)
        seen = []
        rpc.send({"type": "get_state"}, lambda rec: seen.append(rec))
        deadline = time.time() + 5
        while time.time() < deadline and not seen:
            time.sleep(0.02)
        self.assertEqual(len(seen), 1)
        self.assertEqual(seen[0]["command"], "get_state")
        self.assertTrue(seen[0]["id"].startswith("c"))

    def test_unclaimed_failure_reaches_the_hook(self):
        rpc = self.spawn(lambda _r: None)
        self.addCleanup(rpc.stop)
        failures = []
        rpc.on_response = lambda rec, handled: failures.append((rec.get("command"), handled))
        # `compact` is answered success by the fake; a malformed command is
        # answered with success false by the real pi, so drive the hook directly.
        rpc._dispatch_response({"type": "response", "command": "prompt", "success": False,
                                "error": "nope", "id": "nobody"})
        self.assertEqual(failures, [("prompt", False)])

    def test_handler_runs_before_the_hook_and_is_flagged(self):
        rpc = self.spawn(lambda _r: None)
        self.addCleanup(rpc.stop)
        order = []
        rpc.on_response = lambda rec, handled: order.append(("hook", handled))
        rpc._pending["c9"] = lambda rec: order.append(("handler", rec["success"]))
        rpc._dispatch_response({"type": "response", "id": "c9", "success": True})
        self.assertEqual(order, [("handler", True), ("hook", True)])


class GatewayClientTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.addCleanup(self.temp.cleanup)
        session.STATE_DIR = self.temp.name
        session.STATE_FILE = os.path.join(self.temp.name, "supervisor.json")
        session.STATE_PLAIN = os.path.join(self.temp.name, "supervisor.state")
        self.state = {"messages": [{"id": "m1", "chatId": 9, "text": "hi"}],
                      "replies": [], "acked": [], "statuses": [], "not_supervisor": False}
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
                elif self.path == "/api/v1/worker/status":
                    state["statuses"].append((body.get("chatId"), body.get("text"),
                                              body.get("messageId")))
                    self._json(200, {"ok": True, "messageId": body.get("messageId") or 41})
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

    def test_status_returns_the_message_id_to_edit_next_time(self):
        gw = session.Gateway(self.base, "w", "s")
        self.assertEqual(gw.status(9, "working"), 41)
        self.assertEqual(gw.status(9, "still working", 41), 41)
        self.assertEqual(self.state["statuses"], [(9, "working", 0), (9, "still working", 41)])

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
                      "replies": [], "statuses": []}
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
                elif self.path == "/api/v1/worker/status":
                    state["statuses"].append((body.get("chatId"), body.get("text")))
                    self._json(200, {"ok": True, "messageId": 7})
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
            gw,
            lambda chat_id, on_event, on_response: session.PiRpc(
                [sys.executable, str(self.script)], on_event, on_response),
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
        # The tool call the fake pi reported became one activity line.
        self.assertEqual(len(self.state["statuses"]), 3)
        self.assertIn("bash", self.state["statuses"][0][1])
        self.assertTrue(self.state["statuses"][2][1].startswith("✅ done"))


if __name__ == "__main__":
    unittest.main()
