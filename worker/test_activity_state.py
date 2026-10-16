import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("activity_state", Path(__file__).with_name("activity-state.py"))
activity = importlib.util.module_from_spec(spec)
spec.loader.exec_module(activity)


class ActivityStateTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.proc = self.root / "proc"
        boot = self.proc / "sys/kernel/random/boot_id"
        boot.parent.mkdir(parents=True)
        boot.write_text("boot-a")
        self.leases = self.root / "leases"
        self.leases.mkdir()
        self.process(1)

    def process(self, pid, start="123", state="S"):
        folder = self.proc / str(pid)
        folder.mkdir(exist_ok=True)
        fields = [state] + ["0"] * 18 + [start]
        (folder / "stat").write_text(f"{pid} (pi (agent)) " + " ".join(fields))

    def lease(self, name="one", **updates):
        row = dict(version=1, pid=1, startTicks="123", bootId="boot-a", state="busy", updated=100)
        row.update(updates)
        path = self.leases / (name + ".json")
        path.write_text(json.dumps(row))
        return path

    def status(self, now=100):
        return activity.activity_status(self.leases, self.proc, now)

    def test_expiry_and_renewal(self):
        self.lease()
        self.assertEqual(self.status(145), "busy")
        self.assertEqual(self.status(146), "down")
        self.lease(updated=150)
        self.assertEqual(self.status(150), "busy")

    def test_no_fallback_to_children_or_transcripts(self):
        self.process(2)  # a persistent MCP child is irrelevant
        (self.root / "session.jsonl").write_text("recent transcript")
        self.assertEqual(self.status(), "down")
        self.lease(state="idle")
        self.assertEqual(self.status(), "idle")

    def test_concurrent_sessions_any_busy_wins(self):
        self.process(2)
        self.lease(state="idle")
        second = self.lease("two", pid=2)
        self.assertEqual(self.status(), "busy")
        second.unlink()
        self.assertEqual(self.status(), "idle")
        self.lease("stale", pid=2, updated=0)
        self.assertEqual(self.status(), "idle")

    def test_crash_pid_reuse_and_reboot(self):
        for updates in [dict(pid=99), dict(startTicks="old"), dict(bootId="old")]:
            with self.subTest(updates=updates):
                path = self.lease(**updates)
                self.assertEqual(self.status(), "down")
                self.assertFalse(path.exists())
        self.process(1, state="Z")
        self.lease()
        self.assertEqual(self.status(), "down")

    def test_invalid_rows_do_not_hide_valid_idle(self):
        self.lease(state="idle")
        for index, row in enumerate(["broken", "[]", "null", "x" * 5000]):
            (self.leases / f"bad{index}.json").write_text(row)
        for index, updates in enumerate([dict(updated="100"), dict(updated=101),
                                        dict(updated=float("nan")), dict(state="unknown"),
                                        dict(pid="../etc"), dict(version=2)]):
            self.lease(f"invalid{index}", **updates)
        self.assertEqual(self.status(), "idle")

    def test_atomic_temporary_file_is_ignored(self):
        self.lease().rename(self.leases / "one.json.tmp")
        self.assertEqual(self.status(), "down")


if __name__ == "__main__":
    unittest.main()
