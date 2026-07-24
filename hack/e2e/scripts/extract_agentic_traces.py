#!/usr/bin/env python3
# Copyright 2026 The KAITO Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Extract a small, committable trace fixture from the HuggingFace dataset
sammshen/lmcache-agentic-traces for the prefix-cache perf e2e spec.

This is the ONE-TIME, OFFLINE step of "Option A": the full dataset is ~2.37 GB
and must never be fetched at test time. Run this locally to (re)generate the
committed fixture at test/e2e/testdata/agentic-traces.jsonl, then commit the
result.

The output schema matches what test/e2e/utils/traces.go (LoadTraceSessions)
reads: one JSON object per line, one object per LLM iteration, grouped by
session_id, with the cumulative OpenAI-format `input` messages array.

Usage:
    pip install datasets
    python hack/e2e/scripts/extract_agentic_traces.py \\
        --num-sessions 6 \\
        --max-turns 4 \\
        --sources swebench gaia wildclaw \\
        --output test/e2e/testdata/agentic-traces.jsonl

Keep the fixture small: a handful of sessions with a few turns each is enough to
exercise prefix-cache growth. Larger, real-context sessions (median ~21K input
tokens) will produce stronger cache-hit signal but bloat the repo — prefer
regenerating locally for a heavy run rather than committing multi-MB fixtures.
"""

import argparse
import json
import sys
from collections import OrderedDict


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--dataset", default="sammshen/lmcache-agentic-traces",
                   help="HuggingFace dataset id.")
    p.add_argument("--split", default="train", help="Dataset split to read.")
    p.add_argument("--num-sessions", type=int, default=6,
                   help="Total number of sessions to keep in the fixture.")
    p.add_argument("--max-turns", type=int, default=4,
                   help="Keep at most this many (earliest) turns per session.")
    p.add_argument("--sources", nargs="*", default=["swebench", "gaia", "wildclaw"],
                   help="session_id source prefixes to include (balanced round-robin).")
    p.add_argument("--output", default="test/e2e/testdata/agentic-traces.jsonl",
                   help="Output JSONL path.")
    return p.parse_args()


def source_of(session_id: str) -> str:
    # session_id examples: swebench__django__django-16527__claude,
    # gaia__L2_abc123__claude, wildclaw__01_task__claude
    return session_id.split("__", 1)[0]


def main() -> int:
    args = parse_args()
    try:
        from datasets import load_dataset
    except ImportError:
        print("error: pip install datasets", file=sys.stderr)
        return 2

    ds = load_dataset(args.dataset, split=args.split, streaming=True)

    # Group rows by session_id, preserving row order (== turn order).
    by_session: "OrderedDict[str, list]" = OrderedDict()
    wanted_sources = set(args.sources)

    for row in ds:
        sid = row.get("session_id")
        if not sid:
            continue
        src = source_of(sid)
        if wanted_sources and src not in wanted_sources:
            continue
        if sid not in by_session:
            # Keep the first --num-sessions distinct sessions (stream order).
            if len(by_session) >= args.num_sessions:
                continue
            by_session[sid] = []
        by_session[sid].append(row)
        if len(by_session) >= args.num_sessions and all(len(v) >= args.max_turns for v in by_session.values()):
            break

    written = 0
    with open(args.output, "w", encoding="utf-8") as out:
        for sid, rows in list(by_session.items())[: args.num_sessions]:
            for row in rows[: args.max_turns]:
                record = {
                    "session_id": sid,
                    "model": row.get("model", ""),
                    "input": row.get("input", []),
                    "pre_gap": float(row.get("pre_gap", 0.0) or 0.0),
                    "output_length": int(row.get("output_length", 0) or 0),
                }
                out.write(json.dumps(record, ensure_ascii=False) + "\n")
                written += 1

    print(f"wrote {written} rows across {len(by_session)} sessions to {args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
