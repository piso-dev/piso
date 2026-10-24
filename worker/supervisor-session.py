#!/usr/bin/env python3
"""Supervisor session: drain Telegram messages into persistent pi sessions.

The gateway holds the Telegram poller lease and queues authorized messages for
the supervisor. This process is the worker side of that arrangement:

  1. renew the supervisor lease, so the coordinator keeps naming this worker;
  2. long-poll the gateway mailbox for new Telegram messages;
  3. feed each message into a long-lived ``pi --mode rpc`` process.

RPC mode is what makes it interactive rather than one-shot. A message that
arrives while pi is working is a ``steer`` (it joins the running turn), a
``/stop`` is an ``abort`` (the same thing Ctrl-C does), and the session keeps
its context across messages. The gateway never sees pi; it only queues text,
edits one activity line, and posts replies.

Three things make a chat feel like the text box rather than a remote control:

  * every command carries an id and pi's ``response`` is matched against it, so
    a rejected prompt or a failed model call is reported to the chat instead of
    disappearing into the gateway log;
  * tool calls, thinking, retries and compaction become one activity line that
    is edited in place, so a twenty-tool turn reads as progress and not as
    twenty notifications;
  * each chat gets its own pi process started with a stable ``--session-id``,
    so a worker restart resumes the conversation instead of restarting it, and
    two people talking to the bot never share a context.
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

# Telegram caps one message at 4096 characters. Chunking happens here rather
# than at the gateway because the gateway truncates: a long answer must arrive
# in pieces, not lose its tail.
CHUNK_MAX = int(os.environ.get("PISO_SUPERVISOR_CHUNK", "3800"))
# One activity line, edited at most this often. Telegram rate-limits edits and
# a chat does not need to see every token.
STATUS_SECS = float(os.environ.get("PISO_SUPERVISOR_STATUS_SECS", "2"))
STATUS_LINES = int(os.environ.get("PISO_SUPERVISOR_STATUS_LINES", "3"))
STATUS_MAX = int(os.environ.get("PISO_SUPERVISOR_STATUS_MAX", "900"))
# How many chats may have a live pi process at once. Idle sessions are evicted
# least-recently-used; a running turn is never killed to stay under the cap.
MAX_SESSIONS = int(os.environ.get("PISO_SUPERVISOR_MAX_SESSIONS", "4"))
HISTORY_LINES = int(os.environ.get("PISO_SUPERVISOR_HISTORY", "12"))

# Control words a chat can send to drive the session the way the TUI does.
INTERRUPT_WORDS = {"/stop", "/interrupt", "/abort", "/cancel"}
NEW_WORDS = {"/new", "/clear"}

HELP_TEXT = """Send text to talk to pi — it goes straight into the session.

/status   model, thinking level, message count
/history  the last messages in this chat's session
/new      start a fresh session
/compact  compact the context now
/stop     interrupt the running turn
/help     this list

Each chat has its own session, and it survives a worker restart."""


def log(msg):
    sys.stderr.write("piso-supervisor-session: %s\n" % msg)
    sys.stderr.flush()


def ssl_context():
    cafile = os.environ.get("SSL_CERT_FILE")
    if cafile and os.path.exists(cafile):
        return ssl.create_default_context(cafile=cafile)
    return ssl.create_default_context()


def one_line(text, limit):
    """Collapse text to a single short line, for a status line or an error."""
    text = " ".join(str(text or "").split())
    if len(text) <= limit:
        return text
    return text[: limit - 1] + "…"


def format_duration(seconds):
    if seconds <= 0:
        return ""
    if seconds < 10:
        return "%.1fs" % seconds
    if seconds < 60:
        return "%ds" % int(seconds)
    return "%dm%02ds" % (int(seconds) // 60, int(seconds) % 60)


def chunk_text(text, limit=CHUNK_MAX):
    """Split a reply into messages Telegram will accept, preferring line
    boundaries and never dropping the tail."""
    text = (text or "").strip()
    if not text:
        return []
    chunks, current = [], ""
    for line in text.splitlines(True):
        while len(line) > limit:
            if current:
                chunks.append(current)
                current = ""
            chunks.append(line[:limit])
            line = line[limit:]
        if len(current) + len(line) > limit:
            chunks.append(current)
            current = ""
        current += line
    if current:
        chunks.append(current)
    return [c.strip("\n") for c in chunks if c.strip()]


ARG_KEYS = ("command", "path", "file_path", "pattern", "query", "url", "description", "prompt")


def tool_summary(args):
    """The most informative one-liner an arbitrary tool's arguments allow."""
    if isinstance(args, dict):
        for key in ARG_KEYS:
            value = args.get(key)
            if isinstance(value, str) and value.strip():
                return one_line(value, 70)
        if args:
            return one_line(json.dumps(args, sort_keys=True, default=str), 70)
    return ""


def result_text(result):
    if not isinstance(result, dict):
        return ""
    content = result.get("content")
    if isinstance(content, str):
        return content
    parts = []
    for block in content or []:
        if isinstance(block, dict) and block.get("type") == "text":
            parts.append(block.get("text") or "")
    return "\n".join(parts)


def message_text(message):
    content = message.get("content")
    if isinstance(content, str):
        return content
    parts = []
    for block in content or []:
        if isinstance(block, dict) and block.get("type") == "text":
            parts.append(block.get("text") or "")
    return "\n".join(parts)


def history_text(messages, limit=HISTORY_LINES):
    """Render the tail of a pi transcript as chat lines: what /history shows."""
    lines = []
    for message in messages or []:
        role = message.get("role")
        if role not in ("user", "assistant"):
            continue
        text = message_text(message).strip()
        if not text:
            continue
        if len(text) > 1200:
            text = text[:1200] + "…"
        lines.append(("🧑 " if role == "user" else "🤖 ") + text)
    if not lines:
        return "No messages yet."
    return "\n\n".join(lines[-limit:])


def status_text(data):
    """What /status answers: the same facts the TUI footer shows."""
    model = data.get("model") or {}
    bits = [model.get("name") or model.get("id") or "no model"]
    if model.get("provider"):
        bits.append(str(model["provider"]))
    if data.get("thinkingLevel"):
        bits.append("thinking " + str(data["thinkingLevel"]))
    bits.append("%d messages" % (data.get("messageCount") or 0))
    bits.append("working" if data.get("isStreaming") else "idle")
    return "⚙️ " + " · ".join(bits)


def session_id(chat_id):
    """A stable pi session id per chat.

    `--session-id` opens that session if it exists and creates it if not, which
    is exactly the resume semantics a restart needs: the same chat id comes back
    to the same transcript instead of a blank one. Telegram chat ids are
    positive for the private chats this front door serves; the ``m`` keeps the
    id a legal pi session id whatever the sign.
    """
    chat_id = int(chat_id)
    return "tg-m%d" % abs(chat_id) if chat_id < 0 else "tg-%d" % chat_id


def pi_argv(chat_id=None):
    # PISO_PI_CMD is a full-command override for tests and local harnesses; the
    # default runs the real pi in RPC mode.
    override = os.environ.get("PISO_PI_CMD", "").strip()
    if override:
        return shlex.split(override)
    binary = os.environ.get("PISO_PI_BIN", "pi")
    argv = [binary, "--mode", "rpc", "--name", "piso-supervisor"]
    if chat_id is not None:
        argv += ["--session-id", session_id(chat_id)]
    extra = os.environ.get("PISO_PI_ARGS", "")
    if extra:
        argv.extend(shlex.split(extra))
    return argv


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

    def status(self, chat_id, text, message_id=0):
        """Write the chat's one activity line and return the id to edit next
        time. A gateway too old for the route answers 404; None then means
        "stay quiet", so a new worker still works against an old gateway."""
        try:
            out = self._request("POST", "/api/v1/worker/status", {
                "worker": self.worker,
                "chatId": chat_id,
                "text": text[:STATUS_MAX],
                "messageId": message_id or 0,
            })
        except urllib.error.HTTPError as err:
            if err.code in (404, 405):
                return None
            raise
        return int(out.get("messageId") or 0)


class PiRpc:
    """One long-lived ``pi --mode rpc`` child, driven with JSONL on stdin and
    read with JSONL on stdout. Callbacks run on the reader thread."""

    def __init__(self, argv, on_event=None, on_response=None):
        self.on_event = on_event or (lambda _r: None)
        self.on_response = on_response or (lambda _r, _handled=False: None)
        self._pending = {}
        self._seq = 0
        self._state_lock = threading.Lock()
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

    def send(self, cmd, handler=None):
        """Write one command. Pi echoes the id on its ``response``, so a handler
        is correlated instead of guessed at, and an unclaimed response still
        reaches ``on_response``."""
        command = dict(cmd)
        with self._state_lock:
            self._seq += 1
            cid = command.get("id") or "c%d" % self._seq
            command["id"] = cid
            if handler is not None:
                self._pending[cid] = handler
        line = json.dumps(command).encode("utf-8") + b"\n"
        with self._write_lock:
            if self.proc.poll() is not None:
                with self._state_lock:
                    self._pending.pop(cid, None)
                raise BrokenPipeError("pi rpc process has exited")
            self.proc.stdin.write(line)
            self.proc.stdin.flush()
        return cid

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
            if record.get("type") == "response":
                self._dispatch_response(record)
                continue
            try:
                self.on_event(record)
            except Exception as exc:  # a handler bug must not kill the reader
                log("event handler failed: %s" % exc)

    def _dispatch_response(self, record):
        with self._state_lock:
            handler = self._pending.pop(record.get("id"), None)
        if handler is not None:
            try:
                handler(record)
            except Exception as exc:
                log("response handler failed: %s" % exc)
        try:
            self.on_response(record, handler is not None)
        except Exception as exc:
            log("response hook failed: %s" % exc)

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


class Session:
    """One chat's pi process and the state of its current turn.

    The buffer, the activity line and the busy flag are per chat because a chat
    is a conversation: sharing them would make two people talk over each other
    and would answer in whichever chat spoke last.
    """

    def __init__(self, chat_id):
        self.chat_id = chat_id
        self.rpc = None
        self.busy = False
        self.started = 0.0
        self.last_used = time.time()
        # Turn state.
        self.text = []
        self.thinking = ""
        self.tools = []  # [(toolCallId, line)], newest last
        self.tool_count = 0
        # Activity line state.
        self.status_id = 0
        self.status_at = 0.0
        self.pending = None
        self.lock = threading.RLock()

    def alive(self):
        return self.rpc is not None and self.rpc.alive()

    # ---- activity line ----

    def render(self):
        """The text of the chat's activity line. Caller holds the lock."""
        lines = [line for _tid, line in self.tools[-STATUS_LINES:]]
        if self.busy and self.thinking:
            lines = ["💭 " + one_line(self.thinking, 90)] + lines
        if self.busy and not lines:
            lines = ["…"]
        return "\n".join(lines)

    def note(self, line):
        with self.lock:
            self.tools = (self.tools + [(None, line)])[-STATUS_LINES:]
            self.pending = self.render()

    def reset_turn(self):
        with self.lock:
            self.text = []
            self.thinking = ""
            self.tools = []
            self.tool_count = 0
            self.pending = None
            self.status_id = 0
            self.status_at = 0.0


class Supervisor:
    """Owns the lease, the mailbox poll, and one pi session per chat."""

    def __init__(self, gateway, spawn_pi=None, wait=POLL_WAIT):
        self.gateway = gateway
        self.spawn_pi = spawn_pi or default_spawn
        self.wait = wait
        self.sessions = {}
        self.status_ok = True
        self._stop = threading.Event()
        self._is_sup = False
        self._sup_lock = threading.Lock()
        self._sessions_lock = threading.Lock()

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

    # ---- sessions ----

    def session(self, chat_id):
        """The pi session for one chat, started on first use. Every command to
        that chat's pi carries the same `--session-id`, so a restart resumes
        the transcript instead of starting a blank one."""
        with self._sessions_lock:
            sess = self.sessions.get(chat_id)
            if sess is None:
                sess = Session(chat_id)
                self.sessions[chat_id] = sess
                self.start_pi(sess)
                self._evict_locked()
                log("started pi session for chat %s" % chat_id)
            sess.last_used = time.time()
            return sess

    def start_pi(self, sess):
        sess.rpc = self.spawn_pi(
            sess.chat_id,
            lambda rec, s=sess: self.on_event(s, rec),
            lambda rec, handled=False, s=sess: self.on_response(s, rec, handled),
        )

    def _evict_locked(self):
        while len(self.sessions) > MAX_SESSIONS:
            idle = [(s.last_used, chat) for chat, s in self.sessions.items() if not s.busy]
            if not idle:
                return  # never kill a running turn to stay under a cap
            _stamp, chat = min(idle)
            gone = self.sessions.pop(chat)
            if gone.rpc is not None:
                gone.rpc.stop()
            log("evicted idle pi session for chat %s" % chat)

    def revive(self):
        """Restart sessions whose pi process died, keeping their id and any
        answer that finished as it went down."""
        for chat, sess in list(self.sessions.items()):
            if sess.alive():
                continue
            with sess.lock:
                text = "".join(sess.text).strip()
                had_status = sess.status_id != 0
                sess.busy = False
                sess.text = []
                sess.pending = "♻️ pi restarted" if had_status else None
            log("pi session for chat %s exited; restarting it" % chat)
            if had_status:
                # Close out the frozen activity line before the new process
                # takes over, so the chat is not left showing a tool call that
                # will never finish.
                self.flush(sess, force=True)
            if text:
                self.tell(sess, text)
            try:
                self.start_pi(sess)
            except Exception as exc:
                log("restart for chat %s failed: %s" % (chat, exc))

    # ---- activity line ----

    def flush(self, sess, force=False):
        """Push the session's activity line, throttled. Telegram rate-limits
        edits and a chat does not need to see every delta."""
        if not self.status_ok:
            return
        with sess.lock:
            text = sess.pending
            if not text:
                return
            now = time.time()
            if not force and now - sess.status_at < STATUS_SECS:
                return
            sess.pending = None
            sess.status_at = now
            message_id = sess.status_id
        try:
            new_id = self.gateway.status(sess.chat_id, text, message_id)
        except Exception as exc:
            log("status to chat %s failed: %s" % (sess.chat_id, exc))
            return
        if new_id is None:
            self.status_ok = False
            log("gateway has no status route; activity lines are off")
            with sess.lock:
                sess.status_id = 0
                sess.pending = None
            return
        with sess.lock:
            sess.status_id = new_id

    def status_loop(self):
        while not self._stop.wait(STATUS_SECS):
            for sess in list(self.sessions.values()):
                self.flush(sess)

    # ---- pi events ----

    def on_event(self, sess, record):
        kind = record.get("type")
        if kind == "message_update":
            self.on_update(sess, record.get("assistantMessageEvent") or {})
        elif kind == "tool_execution_start":
            self.tool_started(sess, record)
        elif kind == "tool_execution_end":
            self.tool_finished(sess, record)
        elif kind == "message_end":
            self.on_message_end(sess, record.get("message") or {})
        elif kind == "agent_settled":
            self.settled(sess, record)
        elif kind == "compaction_start":
            sess.note("🗜️ compacting context (%s)" % (record.get("reason") or "auto"))
            self.flush(sess)
        elif kind == "compaction_end":
            if record.get("result"):
                sess.note("🗜️ context compacted")
            elif record.get("errorMessage"):
                sess.note("⚠️ compaction failed: " + one_line(record["errorMessage"], 70))
            self.flush(sess, force=True)
        elif kind == "auto_retry_start":
            sess.note("🔁 retrying (%d/%d)" % (record.get("attempt") or 1, record.get("maxAttempts") or 1))
            self.flush(sess, force=True)
        elif kind == "auto_retry_end" and record.get("success") is False:
            sess.note("⚠️ retries exhausted")
            self.flush(sess, force=True)
        elif kind == "session_info_changed":
            log("session name for chat %s is now %r" % (sess.chat_id, record.get("name")))

    def on_update(self, sess, update):
        kind = update.get("type")
        if kind == "text_delta":
            with sess.lock:
                sess.text.append(update.get("delta") or "")
        elif kind == "thinking_start":
            with sess.lock:
                sess.thinking = ""
                sess.pending = sess.render()
            self.flush(sess)
        elif kind == "thinking_delta":
            with sess.lock:
                sess.thinking += update.get("delta") or ""
                sess.pending = sess.render()
            self.flush(sess)
        elif kind == "thinking_end":
            with sess.lock:
                sess.thinking = ""
                sess.pending = sess.render()
            self.flush(sess)

    def on_message_end(self, sess, message):
        if message.get("stopReason") == "error":
            self.tell(sess, "⚠️ " + one_line(message.get("errorMessage") or "the model call failed", 300))

    def tool_started(self, sess, record):
        tool_id = record.get("toolCallId") or ""
        name = record.get("toolName") or "tool"
        summary = tool_summary(record.get("args") or {})
        line = ("🔧 %s %s" % (name, summary)).rstrip()
        with sess.lock:
            sess.tool_count += 1
            sess.tools = (sess.tools + [(tool_id, line)])[-STATUS_LINES:]
            sess.pending = sess.render()
        self.flush(sess)

    def tool_finished(self, sess, record):
        tool_id = record.get("toolCallId") or ""
        name = record.get("toolName") or "tool"
        failed = bool(record.get("isError"))
        took = format_duration((record.get("durationMs") or 0) / 1000.0)
        line = "%s %s%s" % ("❌" if failed else "✅", name, " " + took if took else "")
        if failed:
            detail = one_line(result_text(record.get("result")), 70)
            if detail:
                line += ": " + detail
        with sess.lock:
            sess.tools = [(tid, line if tid == tool_id else old) for tid, old in sess.tools]
            sess.pending = sess.render()
        self.flush(sess, force=True)

    def settled(self, sess, record):
        aborted = bool(record.get("aborted"))
        with sess.lock:
            sess.busy = False
            sess.thinking = ""
            text = "".join(sess.text).strip()
            elapsed = time.time() - sess.started if sess.started else 0.0
            tools = sess.tool_count
            had_status = sess.status_id != 0
            if had_status:
                if aborted:
                    sess.pending = "⏹ aborted · %s" % format_duration(elapsed)
                else:
                    sess.pending = "✅ done · %d tool%s · %s" % (
                        tools, "" if tools == 1 else "s", format_duration(elapsed))
            sess.text = []
            sess.tool_count = 0
        if had_status:
            self.flush(sess, force=True)
        with sess.lock:
            sess.status_id = 0
            sess.status_at = 0.0
            sess.pending = None
            sess.tools = []
        if text:
            self.tell(sess, text)

    # ---- pi responses ----

    def on_response(self, sess, record, handled=False):
        """Catch-all for a response nobody claimed. A rejected prompt must not
        leave the chat looking busy forever."""
        if record.get("success"):
            # A prompt an extension consumed started no run, so no
            # `agent_settled` will ever arrive to clear the busy flag.
            if (record.get("command") == "prompt"
                    and (record.get("data") or {}).get("disposition") == "handled"):
                with sess.lock:
                    sess.busy = False
            return
        if handled:
            return
        command = record.get("command") or "command"
        if command == "prompt":
            with sess.lock:
                sess.busy = False
        self.tell(sess, "⚠️ %s: %s" % (command, one_line(record.get("error") or "rejected", 300)))

    def request(self, sess, cmd, handler=None):
        """Send one command with a correlated handler, reporting a send failure
        to the chat instead of only to the log."""
        rpc = sess.rpc
        if rpc is None:
            self.tell(sess, "⚠️ the pi session is not running; try again in a moment")
            return None
        try:
            return rpc.send(cmd, handler)
        except Exception as exc:
            log("pi command %s failed: %s" % (cmd.get("type"), exc))
            if cmd.get("type") == "prompt":
                with sess.lock:
                    sess.busy = False
            self.tell(sess, "⚠️ %s: %s" % (cmd.get("type"), one_line(exc, 200)))
            return None

    # ---- chat commands ----

    def tell(self, sess, text):
        for chunk in chunk_text(text):
            try:
                self.gateway.reply(sess.chat_id, chunk)
            except Exception as exc:
                log("reply to chat %s failed: %s" % (sess.chat_id, exc))
                return

    def feed(self, message):
        """Route one Telegram message into the chat's session, or run it as one
        of the control words a real session understands."""
        chat = message.get("chatId")
        text = (message.get("text") or "").strip()
        if chat is None or not text:
            return
        sess = self.session(int(chat))
        word = text.lower()
        if word in INTERRUPT_WORDS:
            self.interrupt(sess)
        elif word in NEW_WORDS:
            self.new_session(sess)
        elif word in ("/help", "/commands"):
            self.tell(sess, HELP_TEXT)
        elif word == "/status":
            self.request(sess, {"type": "get_state"}, self.respond_with(sess, self.show_status))
        elif word == "/history" or word.startswith("/history "):
            self.request(sess, {"type": "get_messages"}, self.respond_with(sess, self.show_history))
        elif word == "/compact":
            self.request(sess, {"type": "compact"}, self.respond_with(sess, self.compacted))
        else:
            self.prompt(sess, text)

    def prompt(self, sess, text):
        """While a turn runs a message steers it (delivered after the current
        tool calls); when idle it starts a turn."""
        with sess.lock:
            busy = sess.busy
            if not busy:
                sess.busy = True
                sess.started = time.time()
                sess.text = []
                sess.thinking = ""
                sess.tools = []
                sess.tool_count = 0
                sess.status_id = 0
                sess.status_at = 0.0
                sess.pending = None
        cmd = {"type": "steer", "message": text} if busy else {"type": "prompt", "message": text}
        self.request(sess, cmd)

    def interrupt(self, sess):
        with sess.lock:
            busy = sess.busy
        if not busy:
            self.tell(sess, "Nothing is running.")
            return
        self.request(sess, {"type": "abort"})

    def new_session(self, sess):
        sess.reset_turn()
        self.request(sess, {"type": "new_session"}, self.respond_with(sess, self.started_new))

    def respond_with(self, sess, handler):
        """Bind a per-chat handler to one command's response. Pi answers on a
        shared stdout stream, so the chat has to travel with the request."""
        return lambda record: handler(sess, record)

    def failed(self, sess, record, what):
        """Report a refused command to the chat. A correlated handler answers
        for its own command, so the catch-all leaves `handled` ones alone."""
        if record.get("success"):
            return False
        self.tell(sess, "⚠️ %s: %s" % (what, one_line(record.get("error") or "failed", 300)))
        return True

    def started_new(self, sess, record):
        if self.failed(sess, record, "new session"):
            return
        data = record.get("data") or {}
        if data.get("cancelled"):
            self.tell(sess, "⚠️ new session was cancelled by an extension")
            return
        self.tell(sess, "🆕 new session")

    def show_status(self, sess, record):
        if self.failed(sess, record, "status"):
            return
        self.tell(sess, status_text(record.get("data") or {}))

    def show_history(self, sess, record):
        if self.failed(sess, record, "history"):
            return
        self.tell(sess, history_text((record.get("data") or {}).get("messages")))

    def compacted(self, sess, record):
        if self.failed(sess, record, "compact"):
            return
        data = record.get("data") or {}
        before = data.get("tokensBefore") or 0
        after = data.get("estimatedTokensAfter") or 0
        if before and after:
            self.tell(sess, "🗜️ compacted %s → %s tokens" % (before, after))
        else:
            self.tell(sess, "🗜️ compacted")

    # ---- mailbox ----

    def drain_once(self):
        """One mailbox poll. Returns the number of messages fed."""
        self.revive()
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
        threading.Thread(target=self.status_loop, daemon=True).start()
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
        for sess in list(self.sessions.values()):
            if sess.rpc is not None:
                sess.rpc.stop()


def default_spawn(chat_id, on_event, on_response):
    """One `pi --mode rpc` process for one chat, resumed by its session id."""
    return PiRpc(pi_argv(chat_id), on_event, on_response)


def main():
    base = os.environ.get("GATEWAY_URL", "").strip()
    worker = os.environ.get("PISO_WORKER_NAME", "").strip()
    slug = os.environ.get("PISO_WORKER_SLUG", "").strip()
    if not base or not worker or not slug:
        log("GATEWAY_URL/PISO_WORKER_NAME/PISO_WORKER_SLUG must be set")
        return 3
    gateway = Gateway(base, worker, slug)
    supervisor = Supervisor(gateway)

    def shutdown(_signum, _frame):
        supervisor.stop()
    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    supervisor.run()
    return 0


if __name__ == "__main__":
    sys.exit(main())
