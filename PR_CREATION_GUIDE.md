# 创建 PR-1.1 和 PR-1.2 的步骤

由于权限限制，无法直接从此环境 push 到 kaito-project/production-stack。

以下是创建两个独立 PR 的步骤：

## 方案 A：Fork 后创建 PR（推荐）

### PR-1.1: Skeleton + Dependencies
1. Fork `kaito-project/production-stack` 到你的账户
2. Clone 你的 fork：
   ```bash
   git clone https://github.com/YOUR_USERNAME/production-stack.git
   cd production-stack
   ```

3. 创建 PR-1.1 分支：
   ```bash
   git checkout -b feat/guardrail-phase1-pr-1-1-skeleton
   ```

4. 从这个环境复制代码（或手动创建）：
   - ext_proc/__init__.py
   - ext_proc/server.py
   - requirements.txt
   - Dockerfile
   - setup.py
   - pytest.ini
   - .gitignore
   - tests/test_skeleton.py

5. Commit：
   ```bash
   git add examples/kaito-guard-ext-proc/
   git commit -m "$(cat <<'EOF'
PR-1.1: Skeleton + Dependencies (~80 LOC)

Phase 1.1 foundation: Minimal gRPC ext_proc server stub.

- ext_proc/ module with __init__ and server.py
- ExternalProcessorServicer placeholder (Process() raises NotImplementedError)
- requirements.txt with core dependencies (grpcio, protobuf, pydantic, pyyaml, tiktoken)
- Dockerfile for containerization (python:3.11-slim)
- setup.py for package management
- pytest.ini and test structure
- .gitignore for Python/IDE artifacts

Tests:
- test_version: confirms version is 0.1.0
- test_imports: verifies core dependencies importable
- test_servicer_placeholder: confirms ExternalProcessorServicer exists

All tests pass. Ready for Phase 1.2 (SSE Parser) to start.

Depends on: none
Blocks: PR-1.2 (SSE Parser), PR-1.4 (Scanner Interface)
EOF
)"
   ```

6. Push：
   ```bash
   git push -u origin feat/guardrail-phase1-pr-1-1-skeleton
   ```

7. 在 GitHub web UI 创建 PR：
   - 从 `YOUR_USERNAME:feat/guardrail-phase1-pr-1-1-skeleton`
   - 到 `kaito-project:main`

### PR-1.2: SSE Parser

1. 创建新分支（基于 PR-1.1 分支）：
   ```bash
   git checkout feat/guardrail-phase1-pr-1-1-skeleton
   git pull origin feat/guardrail-phase1-pr-1-1-skeleton  # 同步后的 main
   git checkout -b feat/guardrail-phase1-pr-1-2-sse-parser
   ```

2. 添加 SSE Parser 代码：
   - streaming/sse_parser.py
   - tests/test_sse_parser.py

3. Commit：
   ```bash
   git add examples/kaito-guard-ext-proc/streaming/sse_parser.py examples/kaito-guard-ext-proc/tests/test_sse_parser.py
   git commit -m "$(cat <<'EOF'
PR-1.2: SSE Parser (~100 LOC + 100 tests)

Phase 1.2: Core SSE parsing for OpenAI-compatible LLM streaming.

streaming/sse_parser.py:
- split_sse_events(buf: bytes) → (events, remaining)
  Splits raw bytes on \n\n delimiter, returns complete events + remainder
- parse_sse_event(event: bytes) → Optional[dict]
  Parses "data: {...}" JSON, extracts delta.content from OpenAI format
- is_done_event(event: bytes) → bool
  Detects [DONE] marker for stream termination

Handles:
- Complete events split cleanly on delimiters
- Incomplete events held for next buffer chunk
- Malformed JSON (logs warning, returns None)
- OpenAI delta format with finish_reason, etc.
- Empty/missing content fields
- Non-data lines (comments, etc.)

Tests (16 tests, 100% pass):
- TestSplitSSEEvents: single, multiple, incomplete, empty
- TestParseSSEEvent: delta content, finish_reason, [DONE], edge cases
- TestIsDoneEvent: detection with/without prefix, malformed
- TestIntegration: realistic streaming sequence (chunks end mid-event)

Depends on: PR-1.1
Blocks: PR-1.3 (Logical Stream + Holdback)
EOF
)"
   ```

4. Push：
   ```bash
   git push -u origin feat/guardrail-phase1-pr-1-2-sse-parser
   ```

5. 在 GitHub web UI 创建 PR：
   - 从 `YOUR_USERNAME:feat/guardrail-phase1-pr-1-2-sse-parser`
   - 到 `kaito-project:main`
   - 在 PR 描述中说明："Depends on PR-1.1"

## 方案 B：通过 GitHub Web UI 直接创建

如果你对仓库有 write 权限，可以直接在 GitHub web UI：
1. 创建新分支 `feat/guardrail-phase1-pr-1-1-skeleton`
2. 上传文件
3. 创建 PR
4. 等 PR-1.1 merge 后，再创建 PR-1.2

---

## 提示

- 每个 PR 应该独立
- 按顺序 merge：PR-1.1 → PR-1.2 → PR-1.3 等
- 使用清晰的 commit 消息
- 确保 tests 通过（`pytest tests/`）
