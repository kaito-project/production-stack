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

Partition (shard) mode — split the FULL corpus into shards to run the perf spec
against real data at scale:

    python hack/e2e/scripts/extract_agentic_traces.py \\
        --shards 8 --shard-dir /data/agentic-shards --max-sessions 0

Streams the whole dataset and assigns each whole session to shard-<i>.jsonl in
round-robin order (session k -> shard k % shards), so shards hold a balanced
number of sessions and a shard boundary never falls inside a session — every
file contains only whole, distinct sessions and is independently runnable (e.g.
one container per shard). Point the perf spec at a shard via E2E_TRACE_FIXTURE
(or at a directory/glob of shards, which merges them). --output/--num-sessions/
--max-turns are ignored here.
"""

import argparse
import json
import os
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
                   help="Output JSONL path (single-fixture mode).")
    # Partition (shard) mode: split the full corpus into shard files for
    # running the perf spec against real data. When --shards > 0 the script
    # streams the whole dataset and assigns each whole session to shard-<i>.jsonl
    # round-robin, so every row of a session lands in exactly one shard and
    # shards stay balanced. Point the perf spec at a shard via E2E_TRACE_FIXTURE.
    # --output, --num-sessions and --max-turns are ignored in this mode.
    p.add_argument("--shards", type=int, default=0,
                   help="Number of shard files to partition the full corpus into (0 = single-fixture mode).")
    p.add_argument("--shard-dir", default="test/e2e/testdata/shards",
                   help="Directory to write shard-<i>.jsonl files into (partition mode).")
    p.add_argument("--max-sessions", type=int, default=0,
                   help="Cap total distinct sessions across all shards, and STOP streaming once "
                        "reached (0 = unlimited). Use a small value to sample the corpus quickly.")
    p.add_argument("--input", default="",
                   help="Partition mode only: shard this local JSONL file instead of downloading "
                        "(offline test of the shard path; source filter is skipped).")
    return p.parse_args()


def source_of(session_id: str) -> str:
    # session_id examples: swebench__django__django-16527__claude,
    # gaia__L2_abc123__claude, wildclaw__01_task__claude
    return session_id.split("__", 1)[0]


def make_record(sid: str, row: dict) -> dict:
    return {
        "session_id": sid,
        "model": row.get("model", ""),
        "input": row.get("input", []),
        "pre_gap": float(row.get("pre_gap", 0.0) or 0.0),
        "output_length": int(row.get("output_length", 0) or 0),
    }


def partition(args, ds) -> int:
    """Stream the whole dataset into --shards shard files, grouped by session.

    Every row of a session lands in exactly ONE shard file: the first time a
    session id is seen it is assigned to the next shard in ROUND-ROBIN order
    (session k -> shard k % shards) and that mapping is remembered, so a shard
    boundary NEVER falls inside a session — each file contains only whole,
    distinct sessions. Round-robin (rather than a hash of session_id) keeps the
    session COUNT balanced across shards, so every file has roughly the same
    number of sessions. This is what makes each shard file independently
    runnable — e.g. one container per file — since the perf spec needs >=2
    distinct sessions per file for cross-pod routing.

    Memory-bounded: only open file handles, the small session->shard map (one
    int per session), and per-line buffers are held; rows are written out as
    they stream.
    """
    os.makedirs(args.shard_dir, exist_ok=True)
    wanted_sources = set(args.sources)

    # session_id -> assigned shard index. Doubles as the distinct-session set
    # (enforces --max-sessions) and as the remembered assignment that keeps a
    # session whole even if its rows are not perfectly contiguous. One small
    # entry per session.
    session_shard: dict = {}
    next_shard = 0

    handles = [open(os.path.join(args.shard_dir, f"shard-{i}.jsonl"), "w", encoding="utf-8")
               for i in range(args.shards)]
    per_shard_rows = [0] * args.shards
    per_shard_sessions = [set() for _ in range(args.shards)]
    written = 0
    try:
        for row in ds:
            sid = row.get("session_id")
            if not sid:
                continue
            if wanted_sources and source_of(sid) not in wanted_sources:
                continue
            shard = session_shard.get(sid)
            if shard is None:
                # New session boundary. Cap reached: because the dataset emits
                # each session's rows contiguously, every retained session is
                # already fully written by the time a new session id appears —
                # so we can STOP here instead of streaming the rest of the
                # multi-GB corpus (--max-sessions makes a small, quick sample).
                if args.max_sessions and len(session_shard) >= args.max_sessions:
                    break
                shard = next_shard % args.shards
                session_shard[sid] = shard
                next_shard += 1
            handles[shard].write(json.dumps(make_record(sid, row), ensure_ascii=False) + "\n")
            per_shard_rows[shard] += 1
            per_shard_sessions[shard].add(sid)
            written += 1
    finally:
        for h in handles:
            h.close()

    for i in range(args.shards):
        print(f"shard-{i}.jsonl: {per_shard_rows[i]} rows across {len(per_shard_sessions[i])} sessions")
    print(f"wrote {written} rows across {len(session_shard)} sessions into {args.shards} shards under {args.shard_dir}")
    return 0


def local_jsonl_rows(path: str):
    """Yield rows from a local JSONL file (offline shard-path testing)."""
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            yield json.loads(line)


def main() -> int:
    args = parse_args()

    # Partition (shard) mode.
    if args.shards > 0:
        if args.input:
            # Offline: shard an existing local JSONL file (e.g. the committed
            # fixture) with no download. Local files are pre-curated, so skip
            # the source-prefix filter.
            args.sources = []
            return partition(args, local_jsonl_rows(args.input))
        try:
            from datasets import load_dataset
        except ImportError:
            print("error: pip install datasets", file=sys.stderr)
            return 2
        ds = load_dataset(args.dataset, split=args.split, streaming=True)
        return partition(args, ds)

    # Single-fixture mode.
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
                out.write(json.dumps(make_record(sid, row), ensure_ascii=False) + "\n")
                written += 1

    print(f"wrote {written} rows across {len(by_session)} sessions to {args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
