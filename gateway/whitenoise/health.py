"""Socket/protocol readiness only, not relay or model liveness. No secrets printed."""
import json
import socket
import sys

def ready(path="/run/whitenoise/agent.sock"):
    with socket.socket(socket.AF_UNIX) as conn:
        conn.settimeout(3)
        conn.connect(path)
        conn.sendall(json.dumps({"marmot_agent_control": "marmot.agent-control.v2",
                                "id": "piso-health", "type": "account_list"}).encode() + b"\n")
        with conn.makefile("rb") as reader:
            line = reader.readline(1048577)
        if len(line) > 1048576:
            return False
        out = json.loads(line)
        return (out.get("marmot_agent_control") == "marmot.agent-control.v2"
                and out.get("id") == "piso-health" and out.get("type") == "account_list"
                and isinstance(out.get("accounts"), list))

if __name__ == "__main__":
    try:
        sys.exit(0 if ready() else 1)
    except (OSError, ValueError, AttributeError):
        sys.exit(1)
