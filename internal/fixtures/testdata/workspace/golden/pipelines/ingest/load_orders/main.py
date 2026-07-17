"""load_orders: reads raw.orders_staging, writes analytics.orders (golden sample fixture)."""

import json
import sys


def main() -> None:
    """Answer every engine turn with a bare done frame (a quiet resident)."""
    turn = None
    for line in sys.stdin:
        frame = json.loads(line)
        if frame["event"] == "go":
            turn = frame["turn"]
        elif frame["event"] == "run":
            sys.stdout.write(json.dumps({"event": "done", "turn": turn}) + "\n")
            sys.stdout.flush()


if __name__ == "__main__":
    main()
