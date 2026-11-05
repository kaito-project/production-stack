# OpenAI API compatibility checks

Compares which OpenAI API endpoints two LLM services support: an AI Manager
(production-stack) workload and an Azure AI Foundry deployment. Only API
support is compared, not performance, serving behaviour or model quality.
Each run lives in its own date directory, for example [`20261105/`](20261105/report.md).

## What is tested

80 test cases per service, each sent once:

- 78 HTTP cases in [`scripts/endpoints.cjs`](scripts/endpoints.cjs), groups A–J.
- 2 llmprobe probes (ordinary SSE and JSON SSE) in [`scripts/llmprobe.cjs`](scripts/llmprobe.cjs).

The groups are Models, Chat Completions, llmprobe, legacy Completions,
Responses, stored chat completions, model-type endpoints, resource APIs,
Realtime, and vLLM extensions/runtime paths. Anthropic `/v1/messages` is
excluded.

A case is **Supported** when it returns 2xx and the response has the
protocol shape of that API; generated content is never judged. Resource
APIs are only probed with list calls and nonexistent IDs, so nothing is
created in either account. The rules are spelled out in each report's
Method section.

Three parameter checks are reported separately as compatibility notes:
`max_tokens`, `temperature: 0`, and function tools with
`reasoning_effort: "none"`.

## Requirements

- Node.js 20 or later.
- The [llmprobe](https://github.com/Jwrede/llmprobe) binary. It is
  `llmprobe` on `PATH` by default, or set `LLMPROBE_BIN`.

## Run

Put the endpoints and keys in an env file outside the repository, readable
only by you. The keys never appear in any file the scripts write; they are
redacted from all saved requests and headers.

```bash
umask 077
cat > ~/llm-compare.env <<'EOF'
AIM_BASE=https://<namespace>.<zone>.aksapp.io/v1
AIM_KEY=<api-key>
AIM_MODEL=<deployment-name>
FDY_BASE=https://<resource>.openai.azure.com/openai/v1
FDY_KEY=<api-key>
FDY_MODEL=<deployment-name>
EOF

set -a; . ~/llm-compare.env; set +a
cd docs/openai-api-compatibility
RUN=$(date +%Y%m%d)
for p in aimanager foundry; do
  node scripts/llmprobe.cjs  "$p" "$RUN"
  node scripts/endpoints.cjs "$p" "$RUN"
done
node scripts/report.cjs "$RUN"
```

To re-run selected cases and merge them into an existing run, set
`CASE_FILTER=<case-id>,<case-id>` for `endpoints.cjs`, then rebuild the
report with `report.cjs`.

AI Manager authenticates with `Authorization: Bearer <key>` and Foundry with
`api-key: <key>`. `/v1/...` paths are mapped onto each base URL's path, so
they become `/openai/v1/...` on Foundry. Root paths such as `/health` stay
at the host root.

## Output

```text
<run>/
├── report.md                          # summary + every endpoint and test case (committed)
├── results.json                       # the same results, machine-readable (committed)
├── notes.md                           # optional hand-written run notes, included in the report (committed)
├── endpoints-<provider>.json          # per-provider results (local only)
├── llmprobe-<provider>.json           # (local only)
└── raw/<provider>/                    # per-case evidence (local only)
    ├── <case>.request.json            # request (credentials redacted)
    ├── <case>.response.headers.json
    ├── <case>.response.body
    └── llmprobe/                      # llmprobe config, output, captured traffic
```

Only `report.md`, `results.json` and `notes.md` are committed; [`.gitignore`](.gitignore)
keeps the raw evidence and per-provider files local. `report.cjs` writes the
committed files with the providers' hostnames (and the Foundry resource name)
replaced by `<aimanager-endpoint>`, `<foundry-endpoint>` and `<foundry-account>`.
The raw files keep the real endpoints but never the credentials.

llmprobe's own output also contains timing fields. The report deliberately
ignores them.
