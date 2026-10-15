#!/usr/bin/env python3
"""Supervisor session: drain Telegram messages into one persistent pi session.

The gateway holds the Telegram poller lease and queues authorized messages for
the supervisor. This process is the worker side of that arrangement:

  1. renew the supervisor lease, so the coordinator keeps naming this worker;
  2. long-poll the gateway mailbox for new Telegram messages;
  3. feed each message into a single, long-lived ``pi --mode rpc`` process.

RPC mode is what makes it interactive rather than one-shot. A message that
arrives while pi is working is a ``steer`` (it joins the running turn), a
``/interrupt`` is an ``abort`` (the same thing Ctrl-C does), and the session
keeps its context across messages. The gateway never sees pi; it only queues
text and posts the reply.
"""
import json
import os
import shlex
import signal
import ssl
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

STATE_DIR = os.environ.get("PISO_SUPERVISOR_DIR", "/root/.pi/agent/piso")
STATE_FILE = os.path.join(STATE_DIR, "supervisor.json")
STATE_PLAIN = os.path.join(STATE_DIR, "supervisor.state")
RENEW_SECS = int(os.environ.get("PISO_SUPERVISOR_RENEW_SECS", "40"))
RETRY_SECS = int(os.environ.get("PISO_SUPERVISOR_RETRY_SECS", "20"))
POLL_WAIT = int(os.environ.get("PISO_SUPERVISOR_WAIT", "25"))
REPLY_MAX = 4096

# Control words a chat can send to drive the session the way the TUI does.
INTERRUPT_WORDS = {"/stop", "/interrupt", "/abort", "/cancel"}
NEW_WORDS = {"/new", "/clear"}


def log(msg):
    sys.stderr.write("piso-supervisor-session: %s\n" % msg)
    sys.stderr.flush()


def ssl_context():
    cafile = os.environ.get("SSL_CERT_FILE")
    if cafile and os.path.exists(cafile):
        return ssl.create_default_context(cafile=cafile)
    return ssl.create_default_context()


class NotSupervisor(Exception):
    """The gateway answered that this worker is not the supervisor."""


class Gateway:
    """The worker-facing mailbox API. No credential is held: identity is the
    container's source address, exactly like the other worker watchers."""

    def __init__(self, base, worker, slug, context=None, timeout=10):
        self.base = base.rstrip("/")
        self.worker = worker
        self.slug = slug
        self.context = context if context is not None else ssl_context()
        self.timeout = timeout

    def _request(self, method, path, body=None, timeout=None):
        data = None
        headers = {}
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.timeout, context=self.context) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as err:
            if err.code == 409:
                raise NotSupervisor(err.read().decode("utf-8", "replace"))
            raise
        if not raw:
            return {}
        return json.loads(raw.decode("utf-8"))

    def lease(self):
        return self._request(
            "POST", "/api/v1/worker/supervisor",
            {"worker": self.worker, "slug": self.slug},
        )

    def messages(self, wait):
        query = urllib.parse.urlencode({"worker": self.worker, "wait": wait})
        return self._request(
            "GET", "/api/v1/worker/messages?" + query, timeout=wait + 10,
        )

    def ack(self, ids):
        return self._request(
            "POST", "/api/v1/worker/messages/ack",
            {"worker": self.worker, "ids": list(ids)},
        )

    def reply(self, chat_id, text):
        return self._request(
            "POST", "/api/v1/worker/reply",
            {"worker": self.worker, "chatId": chat_id, "text": text[:REPLY_MAX]},
        )


class PiRpc:
    """One long-lived ``pi --mode rpc`` child, driven with JSONL on stdin and
    read with JSONL on stdout. The event callback runs on the reader thread."""

    def __init__(self, argv, on_event):
        self.on_event = on_event
        self.busy = False
        self._write_lock = threading.Lock()
        self.proc = subprocess.Popen(
            argv,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=None,  # the respawner captures it alongside this process
            bufsize=0,
        )
        self._reader = threading.Thread(target=self._read_loop, daemon=True)
        self._reader.start()

    def send(self, obj):
        line = json.dumps(obj).encode("utf-8") + b"\n"
        with self._write_lock:
            if self.proc.poll() is not None:
                raise BrokenPipeError("pi rpc process has exited")
            self.proc.stdin.write(line)
            self.proc.stdin.flush()

    def _read_loop(self):
        out = self.proc.stdout
        while True:
            raw = out.readline()
            if not raw:
                return
            raw = raw.rstrip(b"\r\n")
            if not raw:
                continue
            try:
                record = json.loads(raw.decode("utf-8"))
            except ValueError:
                log("dropping unparseable pi record: %r" % raw[:200])
                continue
            try:
                self.on_event(record)
            except Exception as exc:  # a handler bug must not kill the reader
                log("event handler failed: %s" % exc)

    def alive(self):
        return self.proc.poll() is None

    def stop(self):
        if self.proc.poll() is None:
            try:
                self.proc.stdin.close()
            except OSError:
                pass
            self.proc.terminate()
            try:
                self.proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=3)


class Supervisor:
    """Owns the lease, the mailbox poll, and one pi session."""

    def __init__(self, gateway, spawn_pi, wait=POLL_WAIT):
        self.gateway = gateway
        self.spawn_pi = spawn_pi
        self.wait = wait
        self.pi = None
        self._stop = threading.Event()
        self._text = []
        self._active_chat = None
        self._is_sup = False
        self._sup_lock = threading.Lock()
        self._text_lock = threading.Lock()

    # ---- lease ----

    def is_supervisor(self):
        with self._sup_lock:
            return self._is_sup

    def _set_supervisor(self, granted, reason=""):
        with self._sup_lock:
            self._is_sup = granted
        plain = "supervisor" if granted else "not-supervisor:%s" % (reason or "unknown")
        try:
            os.makedirs(STATE_DIR, exist_ok=True)
            with open(STATE_PLAIN, "w") as fh:
                fh.write(plain + "\n")
            record = {"worker": self.gateway.worker, "slug": self.gateway.slug,
                      "granted": granted, "reason": reason,
                      "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
            with open(STATE_FILE, "w") as fh:
                fh.write(json.dumps(record) + "\n")
        except OSError as exc:
            log("state write failed: %s" % exc)

    def lease_loop(self):
        while not self._stop.is_set():
            granted = self.lease_once()
            self._stop.wait(RENEW_SECS if granted else RETRY_SECS)

    def lease_once(self):
        """One lease ask. Returns whether it was granted."""
        try:
            grant = self.gateway.lease()
            granted = bool(grant.get("granted"))
            self._set_supervisor(granted, grant.get("reason", ""))
            return granted
        except Exception as exc:
            self._set_supervisor(False, "http-error")
            log("lease failed: %s" % exc)
            return False

    # ---- pi events ----

    def on_event(self, record):
        kind = record.get("type")
        if kind == "message_update":
            update = record.get("assistantMessageEvent") or {}
            if update.get("type") == "text_delta":
                with self._text_lock:
                    self._text.append(update.get("delta", ""))
        elif kind == "agent_settled":
            if self.pi is not None:
                self.pi.busy = False
            self._flush_reply()

    def _flush_reply(self):
        with self._text_lock:
            text = "".join(self._text).strip()
            self._text = []
        chat = self._active_chat
        if not text or chat is None:
            return
        try:
            self.gateway.reply(chat, text)
        except Exception as exc:
            log("reply to chat %s failed: %s" % (chat, exc))

    # ---- messages ----

    def feed(self, message):
        """Route one Telegram message into the live session. A message from a
        new chat becomes the reply target for the turn it joins."""
        text = (message.get("text") or "").strip()
        if not text:
            return
        chat = message.get("chatId")
        if chat is not None:
            self._active_chat = chat
        word = text.lower()
        if word in INTERRUPT_WORDS:
            self.pi.send({"type": "abort"})
            return
        if word in NEW_WORDS:
            self.pi.send({"type": "new_session"})
            return
        # While a turn runs, a new message steers it (delivered after the
        # current tool calls); when idle it starts a turn.
        if self.pi.busy:
            self.pi.send({"type": "steer", "message": text})
        else:
            self.pi.busy = True
            self.pi.send({"type": "prompt", "message": text})

    def drain_once(self):
        """One mailbox poll. Returns the number of messages fed."""
        if self.pi is None or not self.pi.alive():
            self.pi = self.spawn_pi()
            self.pi.on_event = self.on_event
            log("started pi rpc session")
        result = self.gateway.messages(self.wait)
        messages = result.get("messages") or []
        for message in messages:
            self.feed(message)
            try:
                self.gateway.ack([message.get("id")])
            except Exception as exc:
                # A failed ack means redelivery, which is safe; never drop the
                # message locally on the strength of an unconfirmed ack.
                log("ack failed for %s: %s" % (message.get("id"), exc))
        return len(messages)

    def run(self):
        # Take the lease once before polling, so the very first mailbox poll is
        # not refused just because the background renewal has not run yet.
        self.lease_once()
        threading.Thread(target=self.lease_loop, daemon=True).start()
        self.pi = self.spawn_pi()
        self.pi.on_event = self.on_event
        while not self._stop.is_set():
            try:
                self.drain_once()
            except NotSupervisor:
                # The lease moved (or has not landed yet); retry soon, the
                # renewal thread is what actually decides.
                self._set_supervisor(False, "supervisor_taken")
                self._stop.wait(2)
            except Exception as exc:
                log("mailbox poll failed: %s" % exc)
                self._stop.wait(RETRY_SECS)

    def stop(self):
        self._stop.set()
        if self.pi is not None:
            self.pi.stop()


def pi_argv():
    # PISO_PI_CMD is a full-command override for tests and local harnesses; the
    # default runs the real pi in RPC mode.
    override = os.environ.get("PISO_PI_CMD", "").strip()
    if override:
        return shlex.split(override)
    binary = os.environ.get("PISO_PI_BIN", "pi")
    argv = [binary, "--mode", "rpc", "--name", "piso-supervisor"]
    extra = os.environ.get("PISO_PI_ARGS", "")
    if extra:
        argv.extend(shlex.split(extra))
    return argv


def main():
    base = os.environ.get("GATEWAY_URL", "").strip()
    worker = os.environ.get("PISO_WORKER_NAME", "").strip()
    slug = os.environ.get("PISO_WORKER_SLUG", "").strip()
    if not base or not worker or not slug:
        log("GATEWAY_URL/PISO_WORKER_NAME/PISO_WORKER_SLUG must be set")
        return 3
    gateway = Gateway(base, worker, slug)
    supervisor = Supervisor(gateway, lambda: PiRpc(pi_argv(), lambda _r: None))

    def shutdown(_signum, _frame):
        supervisor.stop()
    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    supervisor.run()
    return 0


if __name__ == "__main__":
    sys.exit(main())
