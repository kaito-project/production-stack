# OpenAI API compatibility: AI Manager (production-stack) vs Azure AI Foundry

Run: 2026-11-05. This report compares **OpenAI API support only**. It does not compare performance, serving behaviour or model quality: the two services run different models, so generated content is never judged, and no timing or throughput figures are reported.

## 1. Environment

| | AI Manager (production-stack) | Azure AI Foundry |
| --- | --- | --- |
| Base URL | `https://<aimanager-endpoint>/v1` | `https://<foundry-endpoint>/openai/v1` |
| Deployment (`model`) | `qwen3-8-27b` | `gpt-6-astra` |
| Authentication | `Authorization: Bearer <key>` | `api-key: <key>` |
| Unknown-route reply (GET) | 400: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] | 404: Resource not found |
| llmprobe | llmprobe 1.4.0 | llmprobe 1.4.0 |

## 2. Method

- **80 test cases per service, each run once** (78 HTTP cases plus 2 llmprobe probes), across 59 endpoints in groups A–J. Paths are written as OpenAI paths; `/v1/...` maps onto each base URL (`/openai/v1/...` on Foundry) and vLLM root paths (`/health`, `/tokenize`, …) stay at the host root.
- **Supported** = HTTP 2xx and the response has the protocol shape of that API (e.g. `chat.completion` with `choices[].message`, `finish_reason` and `usage`; SSE `chat.completion.chunk` events ending with `data: [DONE]`; `choices` length 2 for `n=2`; a `logprobs` object; `object: list` with `data[]`). **Not supported** = anything else; the Notes give the HTTP status and the error. What the model generated (content, JSON validity, which tool was called) is not judged.
- **Route only** cases call a list endpoint or a nonexistent ID (`probe-nonexistent`), because no resources are created. They are Supported when the reply is a resource-level not-found / invalid-ID error, i.e. it names the probed ID or differs from the service's reply to a path that does not exist (Section 1). They show that the route exists, not that the full resource lifecycle works.
- **Depends on deployment** marks endpoints whose support depends on the deployed model type (embeddings, images, audio, moderations, vision, realtime, score/rerank/pooling). Both deployments are chat models, so a Not supported there is not a platform gap by itself.
- **Supported (needs model)**: for deployment-dependent endpoints, an error caused only by the deployed model lacking the capability still counts as Supported, because the platform serves the API once a suitable model is deployed. This applies when the request reached the model server and was rejected for the model (AI Manager: an error from vLLM, `x-kaito-error-source: inferenceset`; Foundry: an error naming the model or deployment). Platform-level rejections stay Not supported: AI Manager's gateway fallback or BBR native `400`, EPP's `no parser registered` (the gateway does not route the API), and Foundry's unknown-route `Resource not found`.
- **Not OpenAI API** marks vLLM extensions and runtime paths (group J), kept for completeness. The Anthropic `/v1/messages` paths are excluded.
- **Portable request profile**: every request is identical across services except for the URL and the auth header. Chat uses `max_completion_tokens: 512` and no `temperature`; Responses uses `store: false`; legacy Completions uses `max_tokens` (the only field that API defines). Nothing is created in either account.
- **llmprobe** (ordinary SSE and JSON SSE, 1 round, concurrency 1) runs through a loopback adapter that applies the provider URL and credential and rewrites llmprobe's `max_tokens` to `max_completion_tokens`; responses are streamed back unchanged. Its `validate_json` result is shown in Notes only.

## 3. Summary

| | AI Manager (production-stack) | Azure AI Foundry |
| --- | --- | --- |
| Endpoints supported | 8 / 59 | 31 / 59 |

An endpoint (method + path) is Supported when its base (first) test case is Supported, so an endpoint can be Supported while some of its other test cases fail. Section 4 has the result of every test case.

- **Supported by both (7)**
  - Models
    - `GET /v1/models`
    - `GET /v1/models/{model}`
  - Chat Completions
    - `POST /v1/chat/completions`
  - llmprobe
    - `POST /v1/chat/completions (llmprobe)`
  - Responses
    - `POST /v1/responses`
  - Model-type endpoints
    - `POST /v1/embeddings` — *needs a suitable model on AI Manager (production-stack) and Azure AI Foundry*
    - `POST /v1/chat/completions (image input)` — *depends on deployment*
- **Only AI Manager (production-stack) (1)**
  - Legacy Completions
    - `POST /v1/completions`
- **Only Azure AI Foundry (24)**
  - Responses
    - `GET /v1/responses/{id}` — *route only*
    - `GET /v1/responses/{id}/input_items` — *route only*
    - `DELETE /v1/responses/{id}` — *route only*
    - `POST /v1/responses/{id}/cancel` — *route only*
  - Stored chat completions
    - `GET /v1/chat/completions`
    - `GET /v1/chat/completions/{id}` — *route only*
    - `GET /v1/chat/completions/{id}/messages` — *route only*
  - Model-type endpoints
    - `POST /v1/images/generations` — *needs a suitable model on Azure AI Foundry*
    - `POST /v1/images/edits` — *needs a suitable model on Azure AI Foundry*
    - `POST /v1/audio/speech` — *needs a suitable model on Azure AI Foundry*
    - `POST /v1/audio/transcriptions` — *needs a suitable model on Azure AI Foundry*
    - `POST /v1/audio/translations` — *needs a suitable model on Azure AI Foundry*
  - Resource APIs
    - `GET /v1/files`
    - `GET /v1/files/{id}` — *route only*
    - `GET /v1/files/{id}/content` — *route only*
    - `GET /v1/batches`
    - `GET /v1/batches/{id}` — *route only*
    - `GET /v1/fine_tuning/jobs`
    - `GET /v1/fine_tuning/jobs/{id}` — *route only*
    - `GET /v1/fine_tuning/jobs/{id}/events` — *route only*
    - `GET /v1/fine_tuning/jobs/{id}/checkpoints` — *route only*
    - `GET /v1/vector_stores`
    - `GET /v1/vector_stores/{id}` — *route only*
    - `GET /v1/vector_stores/{id}/files` — *route only*
- **Neither (27)**
  - Model-type endpoints
    - `POST /v1/moderations` — *depends on deployment*
    - `POST /v1/images/variations` — *depends on deployment*
  - Resource APIs
    - `GET /v1/assistants`
    - `GET /v1/assistants/{id}` — *route only*
    - `GET /v1/threads/{id}` — *route only*
    - `GET /v1/threads/{id}/messages` — *route only*
    - `GET /v1/threads/{id}/runs` — *route only*
    - `GET /v1/conversations/{id}` — *route only*
    - `GET /v1/conversations/{id}/items` — *route only*
    - `GET /v1/uploads/{id}` — *route only*
  - Realtime
    - `GET /v1/realtime?model={model}` — *depends on deployment*
  - vLLM extensions & runtime paths
    - `POST /v1/chat/completions/batch` — *not OpenAI API*
    - `POST /v1/score` — *not OpenAI API, depends on deployment*
    - `POST /v1/rerank` — *not OpenAI API, depends on deployment*
    - `GET /health` — *not OpenAI API*
    - `GET /ping` — *not OpenAI API*
    - `GET /version` — *not OpenAI API*
    - `GET /metrics` — *not OpenAI API*
    - `GET /openapi.json` — *not OpenAI API*
    - `GET /docs` — *not OpenAI API*
    - `GET /load` — *not OpenAI API*
    - `GET /server_info` — *not OpenAI API*
    - `POST /tokenize` — *not OpenAI API*
    - `POST /detokenize` — *not OpenAI API*
    - `POST /pooling` — *not OpenAI API, depends on deployment*
    - `POST /score` — *not OpenAI API, depends on deployment*
    - `POST /rerank` — *not OpenAI API, depends on deployment*

Endpoint summary. An endpoint is Supported when its base (first) test case is Supported; Section 4 has every case.

| Group | Endpoint | Base case | AI Manager (production-stack) | Azure AI Foundry | Tags |
| --- | --- | --- | --- | --- | --- |
| A | `GET /v1/models` | `models-list` | ✅ Supported | ✅ Supported |  |
| A | `GET /v1/models/{model}` | `models-get` | ✅ Supported | ✅ Supported |  |
| B | `POST /v1/chat/completions` | `chat` (+16) | ✅ Supported | ✅ Supported |  |
| C | `POST /v1/chat/completions (llmprobe)` | `llmprobe-chat-sse` (+1) | ✅ Supported | ✅ Supported |  |
| D | `POST /v1/completions` | `completions` (+2) | ✅ Supported | ❌ Not supported |  |
| E | `POST /v1/responses` | `responses` (+1) | ✅ Supported | ✅ Supported |  |
| E | `GET /v1/responses/{id}` | `responses-get` | ❌ Not supported | ✅ Supported | route only |
| E | `GET /v1/responses/{id}/input_items` | `responses-input-items` | ❌ Not supported | ✅ Supported | route only |
| E | `DELETE /v1/responses/{id}` | `responses-delete` | ❌ Not supported | ✅ Supported | route only |
| E | `POST /v1/responses/{id}/cancel` | `responses-cancel` | ❌ Not supported | ✅ Supported | route only |
| F | `GET /v1/chat/completions` | `chat-completions-list` | ❌ Not supported | ✅ Supported |  |
| F | `GET /v1/chat/completions/{id}` | `chat-completions-get` | ❌ Not supported | ✅ Supported | route only |
| F | `GET /v1/chat/completions/{id}/messages` | `chat-completions-messages` | ❌ Not supported | ✅ Supported | route only |
| G | `POST /v1/embeddings` | `embeddings` (+1) | ✅ Supported (needs model) | ✅ Supported (needs model) | depends on deployment |
| G | `POST /v1/moderations` | `moderations` | ❌ Not supported | ❌ Not supported | depends on deployment |
| G | `POST /v1/images/generations` | `images-generations` | ❌ Not supported | ✅ Supported (needs model) | depends on deployment |
| G | `POST /v1/images/edits` | `images-edits` | ❌ Not supported | ✅ Supported (needs model) | depends on deployment |
| G | `POST /v1/images/variations` | `images-variations` | ❌ Not supported | ❌ Not supported | depends on deployment |
| G | `POST /v1/audio/speech` | `audio-speech` | ❌ Not supported | ✅ Supported (needs model) | depends on deployment |
| G | `POST /v1/audio/transcriptions` | `audio-transcriptions` | ❌ Not supported | ✅ Supported (needs model) | depends on deployment |
| G | `POST /v1/audio/translations` | `audio-translations` | ❌ Not supported | ✅ Supported (needs model) | depends on deployment |
| G | `POST /v1/chat/completions (image input)` | `chat-vision` | ✅ Supported | ✅ Supported | depends on deployment |
| H | `GET /v1/files` | `files-list` | ❌ Not supported | ✅ Supported |  |
| H | `GET /v1/files/{id}` | `files-get` | ❌ Not supported | ✅ Supported | route only |
| H | `GET /v1/files/{id}/content` | `files-content` | ❌ Not supported | ✅ Supported | route only |
| H | `GET /v1/batches` | `batches-list` | ❌ Not supported | ✅ Supported |  |
| H | `GET /v1/batches/{id}` | `batches-get` | ❌ Not supported | ✅ Supported | route only |
| H | `GET /v1/fine_tuning/jobs` | `fine-tuning-jobs-list` | ❌ Not supported | ✅ Supported |  |
| H | `GET /v1/fine_tuning/jobs/{id}` | `fine-tuning-jobs-get` | ❌ Not supported | ✅ Supported | route only |
| H | `GET /v1/fine_tuning/jobs/{id}/events` | `fine-tuning-events` | ❌ Not supported | ✅ Supported | route only |
| H | `GET /v1/fine_tuning/jobs/{id}/checkpoints` | `fine-tuning-checkpoints` | ❌ Not supported | ✅ Supported | route only |
| H | `GET /v1/assistants` | `assistants-list` | ❌ Not supported | ❌ Not supported |  |
| H | `GET /v1/assistants/{id}` | `assistants-get` | ❌ Not supported | ❌ Not supported | route only |
| H | `GET /v1/vector_stores` | `vector-stores-list` | ❌ Not supported | ✅ Supported |  |
| H | `GET /v1/vector_stores/{id}` | `vector-stores-get` | ❌ Not supported | ✅ Supported | route only |
| H | `GET /v1/vector_stores/{id}/files` | `vector-stores-files` | ❌ Not supported | ✅ Supported | route only |
| H | `GET /v1/threads/{id}` | `threads-get` | ❌ Not supported | ❌ Not supported | route only |
| H | `GET /v1/threads/{id}/messages` | `threads-messages` | ❌ Not supported | ❌ Not supported | route only |
| H | `GET /v1/threads/{id}/runs` | `threads-runs` | ❌ Not supported | ❌ Not supported | route only |
| H | `GET /v1/conversations/{id}` | `conversations-get` | ❌ Not supported | ❌ Not supported | route only |
| H | `GET /v1/conversations/{id}/items` | `conversations-items` | ❌ Not supported | ❌ Not supported | route only |
| H | `GET /v1/uploads/{id}` | `uploads-get` | ❌ Not supported | ❌ Not supported | route only |
| I | `GET /v1/realtime?model={model}` | `realtime-upgrade` | ❌ Not supported | ❌ Not supported | depends on deployment |
| J | `POST /v1/chat/completions/batch` | `chat-batch-endpoint` | ❌ Not supported | ❌ Not supported | not OpenAI API |
| J | `POST /v1/score` | `v1-score` | ❌ Not supported | ❌ Not supported | depends on deployment, not OpenAI API |
| J | `POST /v1/rerank` | `v1-rerank` | ❌ Not supported | ❌ Not supported | depends on deployment, not OpenAI API |
| J | `GET /health` | `health` | ❌ Not supported | ❌ Not supported | not OpenAI API |
| J | `GET /ping` | `ping` | ❌ Not supported | ❌ Not supported | not OpenAI API |
| J | `GET /version` | `version` | ❌ Not supported | ❌ Not supported | not OpenAI API |
| J | `GET /metrics` | `metrics` | ❌ Not supported | ❌ Not supported | not OpenAI API |
| J | `GET /openapi.json` | `openapi-json` | ❌ Not supported | ❌ Not supported | not OpenAI API |
| J | `GET /docs` | `docs` | ❌ Not supported | ❌ Not supported | not OpenAI API |
| J | `GET /load` | `load` | ❌ Not supported | ❌ Not supported | not OpenAI API |
| J | `GET /server_info` | `server-info` | ❌ Not supported | ❌ Not supported | not OpenAI API |
| J | `POST /tokenize` | `tokenize` | ❌ Not supported | ❌ Not supported | not OpenAI API |
| J | `POST /detokenize` | `detokenize` | ❌ Not supported | ❌ Not supported | not OpenAI API |
| J | `POST /pooling` | `pooling` | ❌ Not supported | ❌ Not supported | depends on deployment, not OpenAI API |
| J | `POST /score` | `score` | ❌ Not supported | ❌ Not supported | depends on deployment, not OpenAI API |
| J | `POST /rerank` | `rerank` | ❌ Not supported | ❌ Not supported | depends on deployment, not OpenAI API |

## 4. Results by test case

### A. Models

| Case | Request | AI Manager (production-stack) | Azure AI Foundry | Notes |
| --- | --- | --- | --- | --- |
| `models-list` | `GET /v1/models` | ✅ Supported | ✅ Supported | **AIM 200**: 1 model(s)<br>**Foundry 200**: 252 model(s) |
| `models-get` | `GET /v1/models/{model}` | ✅ Supported | ✅ Supported | **AIM 200**<br>**Foundry 200**: id gpt-6-astra-2026-09-03 |

### B. Chat Completions

| Case | Request | AI Manager (production-stack) | Azure AI Foundry | Notes |
| --- | --- | --- | --- | --- |
| `chat` | `POST /v1/chat/completions` | ✅ Supported | ✅ Supported | **AIM 200**<br>**Foundry 200** |
| `chat-stream` | `POST /v1/chat/completions` | ✅ Supported | ✅ Supported | **AIM 200**<br>**Foundry 200**: 1 extra non-standard chunk(s) (object ""; keys choices,created,id,model,object,prompt_filter_results) |
| `chat-logprobs` | `POST /v1/chat/completions` | ✅ Supported | ❌ Not supported | **AIM 200**<br>**Foundry 400**: Unsupported parameter: 'logprobs' is not supported with this model. |
| `chat-json_object` | `POST /v1/chat/completions` | ✅ Supported | ✅ Supported | **AIM 200**<br>**Foundry 200** |
| `chat-json_object-stream` | `POST /v1/chat/completions` | ✅ Supported | ✅ Supported | **AIM 200**<br>**Foundry 200**: 1 extra non-standard chunk(s) (object ""; keys choices,created,id,model,object,prompt_filter_results) |
| `chat-json_schema` | `POST /v1/chat/completions` | ✅ Supported | ✅ Supported | **AIM 200**<br>**Foundry 200** |
| `chat-json_schema-stream` | `POST /v1/chat/completions` | ✅ Supported | ✅ Supported | **AIM 200**<br>**Foundry 200**: 1 extra non-standard chunk(s) (object ""; keys choices,created,id,model,object,prompt_filter_results) |
| `tools-named` | `POST /v1/chat/completions` | ✅ Supported | ❌ Not supported | **AIM 200**<br>**Foundry 400**: Function tools with reasoning_effort are not supported for gpt-6-astra in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'. |
| `tools-named-stream` | `POST /v1/chat/completions` | ✅ Supported | ❌ Not supported | **AIM 200**<br>**Foundry 400**: Function tools with reasoning_effort are not supported for gpt-6-astra in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'. |
| `tools-required` | `POST /v1/chat/completions` | ✅ Supported | ❌ Not supported | **AIM 200**<br>**Foundry 400**: Function tools with reasoning_effort are not supported for gpt-6-astra in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'. |
| `tools-required-stream` | `POST /v1/chat/completions` | ✅ Supported | ❌ Not supported | **AIM 200**<br>**Foundry 400**: Function tools with reasoning_effort are not supported for gpt-6-astra in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'. |
| `tools-auto` | `POST /v1/chat/completions` | ✅ Supported | ❌ Not supported | **AIM 200**<br>**Foundry 400**: Function tools with reasoning_effort are not supported for gpt-6-astra in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'. |
| `tools-auto-stream` | `POST /v1/chat/completions` | ✅ Supported | ❌ Not supported | **AIM 200**<br>**Foundry 400**: Function tools with reasoning_effort are not supported for gpt-6-astra in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'. |
| `tools-none` | `POST /v1/chat/completions` | ✅ Supported | ❌ Not supported | **AIM 200**: no tool_calls returned (model behaviour, not judged)<br>**Foundry 400**: Function tools with reasoning_effort are not supported for gpt-6-astra in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'. |
| `tools-roundtrip` | `POST /v1/chat/completions` | ✅ Supported | ❌ Not supported | **AIM 200**<br>**Foundry 400**: Function tools with reasoning_effort are not supported for gpt-6-astra in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'. |
| `chat-n2` | `POST /v1/chat/completions` | ✅ Supported | ✅ Supported | **AIM 200**<br>**Foundry 200** |
| `chat-n2-stream` | `POST /v1/chat/completions` | ✅ Supported | ✅ Supported | **AIM 200**<br>**Foundry 200**: 1 extra non-standard chunk(s) (object ""; keys choices,created,id,model,object,prompt_filter_results) |

### C. llmprobe

| Case | Request | AI Manager (production-stack) | Azure AI Foundry | Notes |
| --- | --- | --- | --- | --- |
| `llmprobe-chat-sse` | `POST /v1/chat/completions (llmprobe)` | ✅ Supported | ✅ Supported | **AIM 200**: llmprobe status healthy<br>**Foundry 200**: 1 extra non-standard chunk(s) (object ""; keys choices,created,id,model,object,prompt_filter_results); llmprobe status healthy |
| `llmprobe-json-sse` | `POST /v1/chat/completions (llmprobe)` | ✅ Supported | ✅ Supported | **AIM 200**: llmprobe status healthy<br>**Foundry 200**: 1 extra non-standard chunk(s) (object ""; keys choices,created,id,model,object,prompt_filter_results); llmprobe status healthy |

### D. Legacy Completions

| Case | Request | AI Manager (production-stack) | Azure AI Foundry | Notes |
| --- | --- | --- | --- | --- |
| `completions` | `POST /v1/completions` | ✅ Supported | ❌ Not supported | **AIM 200**<br>**Foundry 400**: The requested operation is unsupported. |
| `completions-stream` | `POST /v1/completions` | ✅ Supported | ❌ Not supported | **AIM 200**<br>**Foundry 400**: The requested operation is unsupported. |
| `completions-batch` | `POST /v1/completions` | ✅ Supported | ❌ Not supported | **AIM 200**<br>**Foundry 400**: The requested operation is unsupported. |

### E. Responses

| Case | Request | AI Manager (production-stack) | Azure AI Foundry | Notes |
| --- | --- | --- | --- | --- |
| `responses` | `POST /v1/responses` | ✅ Supported | ✅ Supported | **AIM 200**<br>**Foundry 200** |
| `responses-stream` | `POST /v1/responses` | ✅ Supported | ✅ Supported | **AIM 200**<br>**Foundry 200** |
| `responses-get` | `GET /v1/responses/{id}` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 400**: route recognized (400: Invalid 'response_id': 'probe-nonexistent'. Expected an ID that begins with 'resp'.) |
| `responses-input-items` | `GET /v1/responses/{id}/input_items` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 400**: route recognized (400: Invalid 'response_id': 'probe-nonexistent'. Expected an ID that begins with 'resp'.) |
| `responses-delete` | `DELETE /v1/responses/{id}` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 400**: route recognized (400: Invalid 'response_id': 'probe-nonexistent'. Expected an ID that begins with 'resp'.) |
| `responses-cancel` | `POST /v1/responses/{id}/cancel` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 400**: route recognized (400: Invalid 'response_id': 'probe-nonexistent'. Expected an ID that begins with 'resp'.) |

### F. Stored chat completions

| Case | Request | AI Manager (production-stack) | Azure AI Foundry | Notes |
| --- | --- | --- | --- | --- |
| `chat-completions-list` | `GET /v1/chat/completions` | ❌ Not supported | ✅ Supported | **AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 200** |
| `chat-completions-get` | `GET /v1/chat/completions/{id}` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: resource-level not-found (404: The specified entity cannot be found.); unknown route replies 404: Resource not found |
| `chat-completions-messages` | `GET /v1/chat/completions/{id}/messages` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: resource-level not-found (404: The specified entity cannot be found.); unknown route replies 404: Resource not found |

### G. Model-type endpoints

| Case | Request | AI Manager (production-stack) | Azure AI Foundry | Notes |
| --- | --- | --- | --- | --- |
| `embeddings` | `POST /v1/embeddings` | ✅ Supported (needs model) | ✅ Supported (needs model) | *depends on deployment*<br>**AIM 404**: platform supports this API; the deployed model does not (404: Not Found [x-kaito-error-source: inferenceset])<br>**Foundry 400**: platform supports this API; the deployed model does not (400: The embeddings operation does not work with the specified model, gpt-6-astra. Please choose different model and try again. You can learn more about which models can be used with each operation here: https://go.microsoft.) |
| `embeddings-batch` | `POST /v1/embeddings` | ✅ Supported (needs model) | ✅ Supported (needs model) | *depends on deployment*<br>**AIM 404**: platform supports this API; the deployed model does not (404: Not Found [x-kaito-error-source: inferenceset])<br>**Foundry 400**: platform supports this API; the deployed model does not (400: The embeddings operation does not work with the specified model, gpt-6-astra. Please choose different model and try again. You can learn more about which models can be used with each operation here: https://go.microsoft.) |
| `moderations` | `POST /v1/moderations` | ❌ Not supported | ❌ Not supported | *depends on deployment*<br>**AIM 400**: inference error: BadRequest - no parser registered matching path suffix for: /v1/moderations [x-kaito-error-source: inferenceset]<br>**Foundry 404**: Resource not found |
| `images-generations` | `POST /v1/images/generations` | ❌ Not supported | ✅ Supported (needs model) | *depends on deployment*<br>**AIM 400**: inference error: BadRequest - no parser registered matching path suffix for: /v1/images/generations [x-kaito-error-source: inferenceset]<br>**Foundry 400**: platform supports this API; the deployed model does not (400: The imageGenerations operation does not work with the specified model, gpt-6-astra. Please choose different model and try again. You can learn more about which models can be used with each operation here: https://go.micr) |
| `images-edits` | `POST /v1/images/edits` | ❌ Not supported | ✅ Supported (needs model) | *depends on deployment*<br>**AIM 400**: inference error: BadRequest - failed to parse request body: invalid character '-' in numeric literal<br>**Foundry 400**: platform supports this API; the deployed model does not (400: Model not supported with Responses API.) |
| `images-variations` | `POST /v1/images/variations` | ❌ Not supported | ❌ Not supported | *depends on deployment*<br>**AIM 400**: inference error: BadRequest - failed to parse request body: invalid character '-' in numeric literal<br>**Foundry 404**: Resource not found |
| `audio-speech` | `POST /v1/audio/speech` | ❌ Not supported | ✅ Supported (needs model) | *depends on deployment*<br>**AIM 400**: inference error: BadRequest - no parser registered matching path suffix for: /v1/audio/speech [x-kaito-error-source: inferenceset]<br>**Foundry 400**: platform supports this API; the deployed model does not (400: The audioSpeech operation does not work with the specified model, gpt-6-astra. Please choose different model and try again. You can learn more about which models can be used with each operation here: https://go.microsoft) |
| `audio-transcriptions` | `POST /v1/audio/transcriptions` | ❌ Not supported | ✅ Supported (needs model) | *depends on deployment*<br>**AIM 400**: inference error: BadRequest - failed to parse request body: invalid character '-' in numeric literal<br>**Foundry 404**: platform supports this API; the deployed model does not (404: The API deployment for this resource does not exist. If you created the deployment within the last 5 minutes, please wait a moment and try again.) |
| `audio-translations` | `POST /v1/audio/translations` | ❌ Not supported | ✅ Supported (needs model) | *depends on deployment*<br>**AIM 400**: inference error: BadRequest - failed to parse request body: invalid character '-' in numeric literal<br>**Foundry 404**: platform supports this API; the deployed model does not (404: The API deployment for this resource does not exist. If you created the deployment within the last 5 minutes, please wait a moment and try again.) |
| `chat-vision` | `POST /v1/chat/completions (image input)` | ✅ Supported | ✅ Supported | *depends on deployment*<br>**AIM 200**<br>**Foundry 200** |

### H. Resource APIs

| Case | Request | AI Manager (production-stack) | Azure AI Foundry | Notes |
| --- | --- | --- | --- | --- |
| `files-list` | `GET /v1/files` | ❌ Not supported | ✅ Supported | **AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 200** |
| `files-get` | `GET /v1/files/{id}` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: resource-level not-found (404: The specified entity cannot be found.); unknown route replies 404: Resource not found |
| `files-content` | `GET /v1/files/{id}/content` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: resource-level not-found (404: The specified entity cannot be found.); unknown route replies 404: Resource not found |
| `batches-list` | `GET /v1/batches` | ❌ Not supported | ✅ Supported | **AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 200** |
| `batches-get` | `GET /v1/batches/{id}` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: route recognized (404: The requested job 'probe-nonexistent' does not exist under the account '<foundry-account>'. Please check the account name, deployment name and th) |
| `fine-tuning-jobs-list` | `GET /v1/fine_tuning/jobs` | ❌ Not supported | ✅ Supported | **AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 200** |
| `fine-tuning-jobs-get` | `GET /v1/fine_tuning/jobs/{id}` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: resource-level not-found (404: The specified entity cannot be found.); unknown route replies 404: Resource not found |
| `fine-tuning-events` | `GET /v1/fine_tuning/jobs/{id}/events` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: resource-level not-found (404: The specified entity cannot be found.); unknown route replies 404: Resource not found |
| `fine-tuning-checkpoints` | `GET /v1/fine_tuning/jobs/{id}/checkpoints` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: resource-level not-found (404: The specified entity cannot be found.); unknown route replies 404: Resource not found |
| `assistants-list` | `GET /v1/assistants` | ❌ Not supported | ❌ Not supported | **AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 404**: Resource not found |
| `assistants-get` | `GET /v1/assistants/{id}` | ❌ Not supported | ❌ Not supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: Resource not found (same reply as an unknown route) |
| `vector-stores-list` | `GET /v1/vector_stores` | ❌ Not supported | ✅ Supported | **AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 200** |
| `vector-stores-get` | `GET /v1/vector_stores/{id}` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 400**: route recognized (400: Invalid 'vector_store_id': 'probe-nonexistent'. Expected an ID that begins with 'vs'.) |
| `vector-stores-files` | `GET /v1/vector_stores/{id}/files` | ❌ Not supported | ✅ Supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 400**: route recognized (400: Invalid 'vector_store_id': 'probe-nonexistent'. Expected an ID that begins with 'vs'.) |
| `threads-get` | `GET /v1/threads/{id}` | ❌ Not supported | ❌ Not supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: Resource not found (same reply as an unknown route) |
| `threads-messages` | `GET /v1/threads/{id}/messages` | ❌ Not supported | ❌ Not supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: Resource not found (same reply as an unknown route) |
| `threads-runs` | `GET /v1/threads/{id}/runs` | ❌ Not supported | ❌ Not supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: Resource not found (same reply as an unknown route) |
| `conversations-get` | `GET /v1/conversations/{id}` | ❌ Not supported | ❌ Not supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: Resource not found (same reply as an unknown route) |
| `conversations-items` | `GET /v1/conversations/{id}/items` | ❌ Not supported | ❌ Not supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: Resource not found (same reply as an unknown route) |
| `uploads-get` | `GET /v1/uploads/{id}` | ❌ Not supported | ❌ Not supported | *route only*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr] (same reply as an unknown route)<br>**Foundry 404**: Resource not found (same reply as an unknown route) |

### I. Realtime

| Case | Request | AI Manager (production-stack) | Azure AI Foundry | Notes |
| --- | --- | --- | --- | --- |
| `realtime-upgrade` | `GET /v1/realtime?model={model}` | ❌ Not supported | ❌ Not supported | *depends on deployment*<br>**AIM no response**: request error: timeout<br>**Foundry 404**: Resource not found |

### J. vLLM extensions & runtime paths

| Case | Request | AI Manager (production-stack) | Azure AI Foundry | Notes |
| --- | --- | --- | --- | --- |
| `chat-batch-endpoint` | `POST /v1/chat/completions/batch` | ❌ Not supported | ❌ Not supported | *not OpenAI API*<br>**AIM 400**: inference error: BadRequest - no parser registered matching path suffix for: /v1/chat/completions/batch [x-kaito-error-source: inferenceset]<br>**Foundry 404**: The specified entity cannot be found. |
| `v1-score` | `POST /v1/score` | ❌ Not supported | ❌ Not supported | *depends on deployment, not OpenAI API*<br>**AIM 400**: inference error: BadRequest - no parser registered matching path suffix for: /v1/score [x-kaito-error-source: inferenceset]<br>**Foundry 404**: Resource not found |
| `v1-rerank` | `POST /v1/rerank` | ❌ Not supported | ❌ Not supported | *depends on deployment, not OpenAI API*<br>**AIM 400**: inference error: BadRequest - no parser registered matching path suffix for: /v1/rerank [x-kaito-error-source: inferenceset]<br>**Foundry 404**: Resource not found |
| `health` | `GET /health` | ❌ Not supported | ❌ Not supported | *not OpenAI API*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 404**: Resource not found |
| `ping` | `GET /ping` | ❌ Not supported | ❌ Not supported | *not OpenAI API*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 404**: Resource not found |
| `version` | `GET /version` | ❌ Not supported | ❌ Not supported | *not OpenAI API*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 404**: Resource not found |
| `metrics` | `GET /metrics` | ❌ Not supported | ❌ Not supported | *not OpenAI API*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 404**: Resource not found |
| `openapi-json` | `GET /openapi.json` | ❌ Not supported | ❌ Not supported | *not OpenAI API*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 404**: Resource not found |
| `docs` | `GET /docs` | ❌ Not supported | ❌ Not supported | *not OpenAI API*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 404**: Resource not found |
| `load` | `GET /load` | ❌ Not supported | ❌ Not supported | *not OpenAI API*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 404**: Resource not found |
| `server-info` | `GET /server_info` | ❌ Not supported | ❌ Not supported | *not OpenAI API*<br>**AIM 400**: The request body did not specify a model; the "model" field is required. [x-kaito-error-source: bbr]<br>**Foundry 404**: Resource not found |
| `tokenize` | `POST /tokenize` | ❌ Not supported | ❌ Not supported | *not OpenAI API*<br>**AIM 400**: inference error: BadRequest - no parser registered matching path suffix for: /tokenize [x-kaito-error-source: inferenceset]<br>**Foundry 404**: Resource not found |
| `detokenize` | `POST /detokenize` | ❌ Not supported | ❌ Not supported | *not OpenAI API*<br>**AIM 400**: inference error: BadRequest - no parser registered matching path suffix for: /detokenize [x-kaito-error-source: inferenceset]<br>**Foundry 404**: Resource not found |
| `pooling` | `POST /pooling` | ❌ Not supported | ❌ Not supported | *depends on deployment, not OpenAI API*<br>**AIM 400**: inference error: BadRequest - no parser registered matching path suffix for: /pooling [x-kaito-error-source: inferenceset]<br>**Foundry 404**: Resource not found |
| `score` | `POST /score` | ❌ Not supported | ❌ Not supported | *depends on deployment, not OpenAI API*<br>**AIM 400**: inference error: BadRequest - no parser registered matching path suffix for: /score [x-kaito-error-source: inferenceset]<br>**Foundry 404**: Resource not found |
| `rerank` | `POST /rerank` | ❌ Not supported | ❌ Not supported | *depends on deployment, not OpenAI API*<br>**AIM 400**: inference error: BadRequest - no parser registered matching path suffix for: /rerank [x-kaito-error-source: inferenceset]<br>**Foundry 404**: Resource not found |

## 5. Compatibility notes

Parameter and field differences found during the run. They do not change the classification above.

| Check | AI Manager (production-stack) | Azure AI Foundry |
| --- | --- | --- |
| Chat with `max_tokens` instead of `max_completion_tokens` | Accepted (200) | Rejected — 400: Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead. |
| Chat with `temperature: 0` | Accepted (200) | Rejected — 400: Unsupported value: 'temperature' does not support 0 with this model. Only the default (1) value is supported. |
| Chat with function tools (`tool_choice: auto`) plus `reasoning_effort: "none"` | Accepted (200) | Rejected — 400: Unsupported value: 'reasoning_effort' does not support 'none' with this model. Supported values are: 'low', 'medium', 'high', and 'xhigh'. |
| llmprobe adapter rewrites applied | max_tokens → max_completion_tokens | max_tokens → max_completion_tokens |
| Non-standard fields in a `chat.completion` response (keys only) | `prompt_logprobs`, `prompt_token_ids`, `prompt_text`, `kv_transfer_params`, `ec_transfer_params`, `metrics`, `choices[].stop_reason`, `choices[].token_ids`, `choices[].routed_experts`, `choices[].message.reasoning` | `prompt_filter_results`, `routing`, `choices[].content_filter_results`, `usage.latency_checkpoint` |
| Non-standard SSE chunks in a chat stream | none | 1 extra non-standard chunk(s) (object ""; keys choices,created,id,model,object,prompt_filter_results) |
| llmprobe `validate_json` (model behaviour, not classified) | llmprobe status healthy | llmprobe status healthy |

## 6. Run notes

- **AI Manager deployment:** InferenceSet `qwen3-8-27b`, preset `Qwen/Qwen3.8-27B` (BF16) on one H100 (`Standard_NC40ads_H100_v5`). KAITO starts vLLM with `--tool-call-parser=qwen3_coder --enable-auto-tool-choice --reasoning-parser=qwen3`, which is why the tool-calling cases are supported. Chat responses carry the model's reasoning in `choices[].message.reasoning`.
- **llmprobe `chat-sse` on AI Manager was run twice.** In the first attempt the request got no response headers within llmprobe's 180 s timeout (`parse stream: context deadline exceeded`). The JSON SSE probe that ran right after it passed, as did every streaming case in the HTTP matrix. The single re-run passed both probes, and the results above use it. The timed-out attempt is kept with the local raw evidence (not committed). Treat it as a one-off stall, not missing support; the cause was not investigated.
- **`realtime-upgrade` on AI Manager** got no reply to the WebSocket upgrade within 180 s, the same as in earlier runs.
- **`/v1/embeddings` on AI Manager** is marked *Supported (needs model)*: the gateway routes the request to vLLM, which returns `404` only because `qwen3-8-27b` is a chat model. Serving embeddings needs an embedding model deployed in vLLM pooling mode (for example `Qwen/Qwen3-Embedding-0.6B` with `runner: pooling` in the InferenceSet's inference ConfigMap). KAITO has no embedding preset in its catalog, and that path has not been verified end to end.

## 7. Evidence and reproduction

- Per-case evidence (requests, response headers and bodies, llmprobe output) is written by the scripts to `raw/` and the per-provider result files next to this report. It stays local and is not committed; see `../.gitignore`. Credentials are redacted in every file.
- Machine-readable results: `results.json`.
- Reproduce: see `../README.md`.
