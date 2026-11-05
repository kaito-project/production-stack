'use strict';
// HTTP test matrix: every case is sent once to one provider and classified as
// Supported or Not supported from the HTTP status and the protocol shape of the
// response only. Generated content is never judged (the providers serve
// different models).
//
// Usage: node endpoints.cjs <aimanager|foundry> <run-dir>
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const https = require('node:https');
const zlib = require('node:zlib');
const { provider, redactor, writer, redactHeaders } = require('./lib.cjs');

const MAX_TOKENS = 512;
const TIMEOUT_MS = 180_000;
const MISSING_ID = 'probe-nonexistent';

// ---------- fixtures ----------
function crc32(buf) {
  let c, crc = 0xffffffff;
  for (const byte of buf) { c = (crc ^ byte) & 0xff; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; crc = (crc >>> 8) ^ c; }
  return (crc ^ 0xffffffff) >>> 0;
}
function pngSquare(size) {
  const chunk = (type, data) => { const len = Buffer.alloc(4); len.writeUInt32BE(data.length); const td = Buffer.concat([Buffer.from(type), data]); const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(td)); return Buffer.concat([len, td, crc]); };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(size, 0); ihdr.writeUInt32BE(size, 4); ihdr[8] = 8; ihdr[9] = 6;
  const row = Buffer.concat([Buffer.from([0]), Buffer.alloc(size * 4, Buffer.from([40, 90, 200, 255]))]);
  const raw = Buffer.concat(Array.from({ length: size }, () => row));
  return Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}
function wavSilence(seconds) {
  const data = 16000 * 2 * seconds;
  const wav = Buffer.alloc(44 + data);
  wav.write('RIFF', 0); wav.writeUInt32LE(36 + data, 4); wav.write('WAVEfmt ', 8); wav.writeUInt32LE(16, 16); wav.writeUInt16LE(1, 20); wav.writeUInt16LE(1, 22);
  wav.writeUInt32LE(16000, 24); wav.writeUInt32LE(32000, 28); wav.writeUInt16LE(2, 32); wav.writeUInt16LE(16, 34); wav.write('data', 36); wav.writeUInt32LE(data, 40);
  return wav;
}
const PNG = pngSquare(64);
const WAV = wavSilence(1);

// ---------- shape checks (protocol only) ----------
const obj = v => v !== null && typeof v === 'object' && !Array.isArray(v);
const chatShape = v => obj(v) && v.object === 'chat.completion' && Array.isArray(v.choices) && v.choices.length > 0 &&
  v.choices.every(c => obj(c.message) && 'finish_reason' in c) && obj(v.usage);
const toolCallsWellFormed = v => v.choices.every(c => c.message.tool_calls == null || (Array.isArray(c.message.tool_calls) &&
  c.message.tool_calls.every(t => typeof t.id === 'string' && t.type === 'function' && typeof t.function?.name === 'string' && typeof t.function?.arguments === 'string')));
const listShape = v => obj(v) && (v.object === 'list' || Array.isArray(v.data)) && Array.isArray(v.data);

function parseSse(text) {
  const events = [];
  let done = false, malformed = 0, current = {};
  for (const line of text.split(/\r?\n/)) {
    if (line.startsWith('event:')) current.event = line.slice(6).trim();
    else if (line.startsWith('data:')) {
      const data = line.slice(5).trim();
      if (data === '[DONE]') { done = true; continue; }
      try { events.push({ event: current.event, data: JSON.parse(data) }); } catch { malformed++; }
      current = {};
    }
  }
  return { events, done, malformed };
}

const fail = note => ({ ok: false, note });
const pass = (note = '') => ({ ok: true, note });

function chatStreamCheck(r, { indexes } = {}) {
  if (!/text\/event-stream/.test(r.headers['content-type'] || '')) return fail(`content-type ${r.headers['content-type']}`);
  const { events, done, malformed } = parseSse(r.text);
  if (malformed) return fail(`${malformed} SSE data lines are not JSON`);
  if (!done) return fail('stream did not end with data: [DONE]');
  const chunks = events.map(e => e.data);
  const standard = chunks.filter(c => c.object === 'chat.completion.chunk');
  const extra = chunks.filter(c => c.object !== 'chat.completion.chunk');
  if (!standard.length) return fail('no chat.completion.chunk events');
  if (extra.some(c => Array.isArray(c.choices) && c.choices.length)) return fail('chunks carrying choices with a non-standard object type');
  if (indexes) {
    const seen = new Set(standard.flatMap(c => (c.choices || []).map(ch => ch.index)));
    if (!indexes.every(i => seen.has(i))) return fail(`choice indexes seen: ${[...seen].join(',') || 'none'}`);
  }
  return pass(extra.length ? `${extra.length} extra non-standard chunk(s) (object ${JSON.stringify(extra[0].object)}; keys ${Object.keys(extra[0]).join(',')})` : '');
}
function completionsStreamCheck(r) {
  if (!/text\/event-stream/.test(r.headers['content-type'] || '')) return fail(`content-type ${r.headers['content-type']}`);
  const { events, done, malformed } = parseSse(r.text);
  if (malformed) return fail(`${malformed} SSE data lines are not JSON`);
  if (!done) return fail('stream did not end with data: [DONE]');
  return events.length && events.every(e => e.data.object === 'text_completion') ? pass() : fail('chunks are not text_completion objects');
}
function responsesStreamCheck(r) {
  if (!/text\/event-stream/.test(r.headers['content-type'] || '')) return fail(`content-type ${r.headers['content-type']}`);
  const { events, malformed } = parseSse(r.text);
  if (malformed) return fail(`${malformed} SSE data lines are not JSON`);
  const types = events.map(e => e.data.type);
  if (!types.length || !types.every(t => typeof t === 'string' && t.startsWith('response.'))) return fail('events are not response.* events');
  return types.some(t => t === 'response.completed' || t === 'response.incomplete') ? pass() : fail(`no response.completed/incomplete event (last: ${types.at(-1)})`);
}
// Q14: a route-only probe (nonexistent ID) is Supported when the service answers
// with a not-found / invalid-ID error for that resource. The reference is the
// service's own reply to a path that does not exist (same method): a reply that
// names the probed ID, or that differs from that unknown-route reply, means the
// route exists. The unknown-route reply itself (e.g. Foundry's generic
// "Resource not found", or the AI Manager gateway's fallback) means it does not.
const UNKNOWN_ROUTE = '/v1/probe-no-such-route';
const errorMessage = r => {
  const e = r.json?.error;
  return String((obj(e) ? e.message ?? e.code : e) ?? r.json?.message ?? r.json?.detail ?? r.text).slice(0, 300);
};
function routeOnlyCheck(r, unknown) {
  if (r.status >= 200 && r.status < 300) return pass('2xx for a nonexistent ID');
  if (r.status === 0) return fail(errorNote(r));
  if (String(JSON.stringify(r.json ?? r.text)).includes(MISSING_ID)) return pass(`route recognized (${r.status}: ${errorMessage(r).slice(0, 140)})`);
  if (unknown && r.status === unknown.status && errorMessage(r) === errorMessage(unknown)) return fail(`${errorNote(r)} (same reply as an unknown route)`);
  if (r.status >= 400 && r.status < 500 && r.json !== undefined) return pass(`resource-level not-found (${r.status}: ${errorMessage(r).slice(0, 120)}); unknown route replies ${unknown?.status}: ${unknown ? errorMessage(unknown).slice(0, 60) : 'n/a'}`);
  return fail(errorNote(r));
}

function errorNote(r) {
  const e = r.json?.error;
  const message = obj(e) ? e.message ?? e.code : typeof e === 'string' ? e : r.json?.message ?? r.json?.detail ?? r.text.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim();
  const source = r.headers['x-kaito-error-source'] ? ` [x-kaito-error-source: ${r.headers['x-kaito-error-source']}]` : '';
  return `${r.status}: ${String(message ?? '').slice(0, 220)}${source}`;
}

// ---------- case table ----------
function buildCases(model) {
  const cases = [];
  const add = (id, group, method, p, opts) => cases.push({ id, group, method, path: p, ...opts });
  const say = 'Say hello in one short sentence.';
  const chat = (extra = {}) => ({ model, messages: [{ role: 'user', content: say }], max_completion_tokens: MAX_TOKENS, ...extra });
  const weatherTool = [{ type: 'function', function: { name: 'get_weather', description: 'Get the current weather for a city.', parameters: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'] } } }];
  const toolChat = (choice, extra = {}) => chat({ messages: [{ role: 'user', content: 'What is the weather in Paris?' }], tools: weatherTool, tool_choice: choice, ...extra });
  const schema = { type: 'json_schema', json_schema: { name: 'city', strict: true, schema: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'], additionalProperties: false } } };
  const jsonPrompt = [{ role: 'user', content: 'Return a JSON object with a "city" field set to Paris.' }];
  const chatCheck = r => chatShape(r.json) ? pass() : fail('response is not a chat.completion with choices[].message, finish_reason and usage');
  const toolCheck = r => chatShape(r.json) && toolCallsWellFormed(r.json) ? pass(r.json.choices[0].message.tool_calls ? '' : 'no tool_calls returned (model behaviour, not judged)') : fail('not a chat.completion, or tool_calls malformed');
  const listCheck = r => listShape(r.json) ? pass() : fail('not a list object with data[]');

  // A. Models
  add('models-list', 'A. Models', 'GET', '/v1/models', { check: r => listShape(r.json) && r.json.data.every(m => typeof m.id === 'string') ? pass(`${r.json.data.length} model(s)`) : fail('not a list of models') });
  add('models-get', 'A. Models', 'GET', `/v1/models/${model}`, { template: '/v1/models/{model}', check: r => obj(r.json) && typeof r.json.id === 'string' ? pass(r.json.id === model ? '' : `id ${r.json.id}`) : fail('not a model object') });

  // B. Chat Completions
  const C = '/v1/chat/completions';
  add('chat', 'B. Chat Completions', 'POST', C, { body: chat(), check: chatCheck, compat: true });
  add('chat-stream', 'B. Chat Completions', 'POST', C, { body: chat({ stream: true }), check: r => chatStreamCheck(r) });
  add('chat-logprobs', 'B. Chat Completions', 'POST', C, { body: chat({ logprobs: true, top_logprobs: 2 }), check: r => chatShape(r.json) && obj(r.json.choices[0].logprobs) ? pass() : fail('choices[].logprobs missing') });
  add('chat-json_object', 'B. Chat Completions', 'POST', C, { body: chat({ messages: jsonPrompt, response_format: { type: 'json_object' } }), check: chatCheck });
  add('chat-json_object-stream', 'B. Chat Completions', 'POST', C, { body: chat({ messages: jsonPrompt, response_format: { type: 'json_object' }, stream: true }), check: r => chatStreamCheck(r) });
  add('chat-json_schema', 'B. Chat Completions', 'POST', C, { body: chat({ messages: jsonPrompt, response_format: schema }), check: chatCheck });
  add('chat-json_schema-stream', 'B. Chat Completions', 'POST', C, { body: chat({ messages: jsonPrompt, response_format: schema, stream: true }), check: r => chatStreamCheck(r) });
  for (const [name, choice] of [['named', { type: 'function', function: { name: 'get_weather' } }], ['required', 'required'], ['auto', 'auto']]) {
    add(`tools-${name}`, 'B. Chat Completions', 'POST', C, { body: toolChat(choice), check: toolCheck });
    add(`tools-${name}-stream`, 'B. Chat Completions', 'POST', C, { body: toolChat(choice, { stream: true }), check: r => chatStreamCheck(r) });
  }
  add('tools-none', 'B. Chat Completions', 'POST', C, { body: toolChat('none'), check: toolCheck });
  add('tools-roundtrip', 'B. Chat Completions', 'POST', C, {
    body: chat({
      tools: weatherTool, messages: [
        { role: 'user', content: 'What is the weather in Paris?' },
        { role: 'assistant', content: null, tool_calls: [{ id: 'call_probe_1', type: 'function', function: { name: 'get_weather', arguments: '{"city":"Paris"}' } }] },
        { role: 'tool', tool_call_id: 'call_probe_1', content: '{"temperature_c":18,"condition":"sunny"}' },
      ],
    }), check: chatCheck,
  });
  add('chat-n2', 'B. Chat Completions', 'POST', C, { body: chat({ n: 2 }), check: r => chatShape(r.json) && r.json.choices.length === 2 ? pass() : fail(`choices length ${r.json?.choices?.length ?? 'n/a'}, expected 2`) });
  add('chat-n2-stream', 'B. Chat Completions', 'POST', C, { body: chat({ n: 2, stream: true }), check: r => chatStreamCheck(r, { indexes: [0, 1] }) });

  // D. Legacy Completions (the API only defines max_tokens)
  const text = { model, prompt: 'Say hello', max_tokens: 64 };
  add('completions', 'D. Legacy Completions', 'POST', '/v1/completions', { body: text, check: r => obj(r.json) && r.json.object === 'text_completion' && Array.isArray(r.json.choices) && r.json.choices.every(c => 'text' in c) ? pass() : fail('not a text_completion') });
  add('completions-stream', 'D. Legacy Completions', 'POST', '/v1/completions', { body: { ...text, stream: true }, check: completionsStreamCheck });
  add('completions-batch', 'D. Legacy Completions', 'POST', '/v1/completions', { body: { ...text, prompt: ['Say hello', 'Say goodbye'] }, check: r => r.json?.object === 'text_completion' && r.json.choices?.length === 2 ? pass() : fail(`choices length ${r.json?.choices?.length ?? 'n/a'}, expected 2`) });

  // E. Responses (store: false — nothing is persisted)
  const resp = { model, input: say, max_output_tokens: MAX_TOKENS, store: false };
  add('responses', 'E. Responses', 'POST', '/v1/responses', { body: resp, check: r => obj(r.json) && r.json.object === 'response' && Array.isArray(r.json.output) ? pass() : fail('not a response object with output[]') });
  add('responses-stream', 'E. Responses', 'POST', '/v1/responses', { body: { ...resp, stream: true }, check: responsesStreamCheck });
  add('responses-get', 'E. Responses', 'GET', `/v1/responses/${MISSING_ID}`, { routeOnly: true });
  add('responses-input-items', 'E. Responses', 'GET', `/v1/responses/${MISSING_ID}/input_items`, { routeOnly: true });
  add('responses-delete', 'E. Responses', 'DELETE', `/v1/responses/${MISSING_ID}`, { routeOnly: true });
  add('responses-cancel', 'E. Responses', 'POST', `/v1/responses/${MISSING_ID}/cancel`, { routeOnly: true });

  // F. Stored chat completions
  add('chat-completions-list', 'F. Stored chat completions', 'GET', C, { check: listCheck });
  add('chat-completions-get', 'F. Stored chat completions', 'GET', `${C}/${MISSING_ID}`, { routeOnly: true });
  add('chat-completions-messages', 'F. Stored chat completions', 'GET', `${C}/${MISSING_ID}/messages`, { routeOnly: true });

  // G. Model-type endpoints (result depends on the deployed model)
  const dep = { deploymentDependent: true };
  add('embeddings', 'G. Model-type endpoints', 'POST', '/v1/embeddings', { ...dep, body: { model, input: 'Hello' }, check: r => listShape(r.json) && r.json.data.length === 1 && r.json.data.every(d => d.object === 'embedding') ? pass() : fail('not a list of embedding objects') });
  add('embeddings-batch', 'G. Model-type endpoints', 'POST', '/v1/embeddings', { ...dep, body: { model, input: ['Hello', 'World'] }, check: r => listShape(r.json) && r.json.data.length === 2 && r.json.data.every(d => d.object === 'embedding') ? pass() : fail('not a list of 2 embedding objects') });
  add('moderations', 'G. Model-type endpoints', 'POST', '/v1/moderations', { ...dep, body: { model, input: 'Hello' }, check: r => obj(r.json) && Array.isArray(r.json.results) ? pass() : fail('no results[]') });
  const imageCheck = r => obj(r.json) && Array.isArray(r.json.data) && r.json.data.length > 0 && r.json.data.every(d => d.url || d.b64_json) ? pass() : fail('no data[] with url/b64_json');
  add('images-generations', 'G. Model-type endpoints', 'POST', '/v1/images/generations', { ...dep, body: { model, prompt: 'A small blue square', n: 1, size: '1024x1024' }, check: imageCheck });
  add('images-edits', 'G. Model-type endpoints', 'POST', '/v1/images/edits', { ...dep, multipart: { model, prompt: 'Make the square red.', image: { filename: 'square.png', type: 'image/png', data: PNG } }, check: imageCheck });
  add('images-variations', 'G. Model-type endpoints', 'POST', '/v1/images/variations', { ...dep, multipart: { model, image: { filename: 'square.png', type: 'image/png', data: PNG } }, check: imageCheck });
  add('audio-speech', 'G. Model-type endpoints', 'POST', '/v1/audio/speech', { ...dep, body: { model, input: 'Hello world.', voice: 'alloy' }, check: r => /^(audio\/|application\/octet-stream)/.test(r.headers['content-type'] || '') && r.raw.length > 0 ? pass() : fail(`content-type ${r.headers['content-type']}`) });
  for (const op of ['transcriptions', 'translations']) {
    add(`audio-${op}`, 'G. Model-type endpoints', 'POST', `/v1/audio/${op}`, { ...dep, multipart: { model, file: { filename: 'silence.wav', type: 'audio/wav', data: WAV } }, check: r => obj(r.json) && typeof r.json.text === 'string' ? pass() : fail('no text field') });
  }
  add('chat-vision', 'G. Model-type endpoints', 'POST', C, { ...dep, body: chat({ messages: [{ role: 'user', content: [{ type: 'text', text: 'Describe this image.' }, { type: 'image_url', image_url: { url: `data:image/png;base64,${PNG.toString('base64')}` } }] }] }), check: chatCheck });

  // H. Resource APIs: list calls and nonexistent-ID probes only (nothing is created)
  const beta = { 'OpenAI-Beta': 'assistants=v2' };
  const H = 'H. Resource APIs';
  add('files-list', H, 'GET', '/v1/files', { check: listCheck });
  add('files-get', H, 'GET', `/v1/files/${MISSING_ID}`, { routeOnly: true });
  add('files-content', H, 'GET', `/v1/files/${MISSING_ID}/content`, { routeOnly: true });
  add('batches-list', H, 'GET', '/v1/batches', { check: listCheck });
  add('batches-get', H, 'GET', `/v1/batches/${MISSING_ID}`, { routeOnly: true });
  add('fine-tuning-jobs-list', H, 'GET', '/v1/fine_tuning/jobs', { check: listCheck });
  add('fine-tuning-jobs-get', H, 'GET', `/v1/fine_tuning/jobs/${MISSING_ID}`, { routeOnly: true });
  add('fine-tuning-events', H, 'GET', `/v1/fine_tuning/jobs/${MISSING_ID}/events`, { routeOnly: true });
  add('fine-tuning-checkpoints', H, 'GET', `/v1/fine_tuning/jobs/${MISSING_ID}/checkpoints`, { routeOnly: true });
  add('assistants-list', H, 'GET', '/v1/assistants', { headers: beta, check: listCheck });
  add('assistants-get', H, 'GET', `/v1/assistants/${MISSING_ID}`, { headers: beta, routeOnly: true });
  add('vector-stores-list', H, 'GET', '/v1/vector_stores', { headers: beta, check: listCheck });
  add('vector-stores-get', H, 'GET', `/v1/vector_stores/${MISSING_ID}`, { headers: beta, routeOnly: true });
  add('vector-stores-files', H, 'GET', `/v1/vector_stores/${MISSING_ID}/files`, { headers: beta, routeOnly: true });
  add('threads-get', H, 'GET', `/v1/threads/${MISSING_ID}`, { headers: beta, routeOnly: true });
  add('threads-messages', H, 'GET', `/v1/threads/${MISSING_ID}/messages`, { headers: beta, routeOnly: true });
  add('threads-runs', H, 'GET', `/v1/threads/${MISSING_ID}/runs`, { headers: beta, routeOnly: true });
  add('conversations-get', H, 'GET', `/v1/conversations/${MISSING_ID}`, { routeOnly: true });
  add('conversations-items', H, 'GET', `/v1/conversations/${MISSING_ID}/items`, { routeOnly: true });
  add('uploads-get', H, 'GET', `/v1/uploads/${MISSING_ID}`, { routeOnly: true });

  // I. Realtime (WebSocket upgrade)
  add('realtime-upgrade', 'I. Realtime', 'GET', `/v1/realtime?model=${encodeURIComponent(model)}`, { template: '/v1/realtime?model={model}', websocket: true, ...dep, check: r => r.status === 101 ? pass('101 Switching Protocols') : fail(errorNote(r)) });

  // J. vLLM extensions and runtime paths (not OpenAI API)
  const ext = { extension: true };
  const J = 'J. vLLM extensions & runtime paths';
  const anyJson = r => r.json !== undefined ? pass() : fail('response is not JSON');
  add('chat-batch-endpoint', J, 'POST', `${C}/batch`, { ...ext, body: chat({ messages: [[{ role: 'user', content: say }], [{ role: 'user', content: say }]] }), check: r => (Array.isArray(r.json) && r.json.length === 2 && r.json.every(chatShape)) || (chatShape(r.json) && r.json.choices.length === 2) ? pass() : fail('not 2 chat completions') });
  const scoreCheck = r => obj(r.json) && Array.isArray(r.json.data) && r.json.data.every(d => typeof d.score === 'number') ? pass() : fail('no data[].score');
  const rerankCheck = r => obj(r.json) && Array.isArray(r.json.results) && r.json.results.every(d => typeof d.relevance_score === 'number') ? pass() : fail('no results[].relevance_score');
  const scoreBody = { model, text_1: 'hello', text_2: 'hello world' };
  const rerankBody = { model, query: 'hello', documents: ['hello world', 'goodbye'] };
  add('v1-score', J, 'POST', '/v1/score', { ...ext, ...dep, body: scoreBody, check: scoreCheck });
  add('v1-rerank', J, 'POST', '/v1/rerank', { ...ext, ...dep, body: rerankBody, check: rerankCheck });
  add('health', J, 'GET', '/health', { ...ext, check: () => pass() });
  add('ping', J, 'GET', '/ping', { ...ext, check: () => pass() });
  add('version', J, 'GET', '/version', { ...ext, check: r => typeof r.json?.version === 'string' ? pass(`version ${r.json.version}`) : fail('no version field') });
  add('metrics', J, 'GET', '/metrics', { ...ext, check: r => /^# (HELP|TYPE) /m.test(r.text) ? pass() : fail('not Prometheus text') });
  add('openapi-json', J, 'GET', '/openapi.json', { ...ext, check: r => r.json?.openapi || r.json?.paths ? pass() : fail('not an OpenAPI document') });
  add('docs', J, 'GET', '/docs', { ...ext, check: r => /text\/html/.test(r.headers['content-type'] || '') ? pass() : fail(`content-type ${r.headers['content-type']}`) });
  add('load', J, 'GET', '/load', { ...ext, check: anyJson });
  add('server-info', J, 'GET', '/server_info', { ...ext, check: anyJson });
  add('tokenize', J, 'POST', '/tokenize', { ...ext, body: { model, prompt: 'Hello world' }, check: r => Array.isArray(r.json?.tokens) ? pass() : fail('no tokens[]') });
  add('detokenize', J, 'POST', '/detokenize', { ...ext, body: { model, tokens: [9906, 1917] }, check: r => typeof r.json?.prompt === 'string' ? pass() : fail('no prompt field') });
  add('pooling', J, 'POST', '/pooling', { ...ext, ...dep, body: { model, input: 'Hello' }, check: r => obj(r.json) && Array.isArray(r.json.data) ? pass() : fail('no data[]') });
  add('score', J, 'POST', '/score', { ...ext, ...dep, body: scoreBody, check: scoreCheck });
  add('rerank', J, 'POST', '/rerank', { ...ext, ...dep, body: rerankBody, check: rerankCheck });

  for (const c of cases) {
    if (c.routeOnly) c.check = routeOnlyCheck;
    c.template ??= c.path.replace(MISSING_ID, '{id}');
    c.endpoint = `${c.method} ${c.template}`;
  }
  return cases;
}

// Compatibility notes: native OpenAI parameters the matrix deliberately avoids.
function compatCases(model) {
  const base = { model, messages: [{ role: 'user', content: 'Say hello in one short sentence.' }] };
  return [
    { id: 'compat-max_tokens', note: 'Chat with `max_tokens` instead of `max_completion_tokens`', method: 'POST', path: '/v1/chat/completions', body: { ...base, max_tokens: MAX_TOKENS } },
    { id: 'compat-temperature0', note: 'Chat with `temperature: 0`', method: 'POST', path: '/v1/chat/completions', body: { ...base, max_completion_tokens: MAX_TOKENS, temperature: 0 } },
    { id: 'compat-tools-reasoning_effort-none', note: 'Chat with function tools (`tool_choice: auto`) plus `reasoning_effort: "none"`', method: 'POST', path: '/v1/chat/completions',
      body: { model, messages: [{ role: 'user', content: 'What is the weather in Paris?' }], max_completion_tokens: MAX_TOKENS, reasoning_effort: 'none', tool_choice: 'auto',
        tools: [{ type: 'function', function: { name: 'get_weather', description: 'Get the current weather for a city.', parameters: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'] } } }] } },
  ];
}

// ---------- transport ----------
function multipartBody(fields) {
  const boundary = `----llmprobe${Math.random().toString(16).slice(2)}`;
  const parts = [];
  for (const [name, value] of Object.entries(fields)) {
    if (obj(value) && Buffer.isBuffer(value.data)) {
      parts.push(Buffer.from(`--${boundary}\r\nContent-Disposition: form-data; name="${name}"; filename="${value.filename}"\r\nContent-Type: ${value.type}\r\n\r\n`), value.data, Buffer.from('\r\n'));
    } else parts.push(Buffer.from(`--${boundary}\r\nContent-Disposition: form-data; name="${name}"\r\n\r\n${value}\r\n`));
  }
  parts.push(Buffer.from(`--${boundary}--\r\n`));
  return { body: Buffer.concat(parts), contentType: `multipart/form-data; boundary=${boundary}` };
}

function send(p, c) {
  const url = p.url(c.path);
  const headers = { ...p.headers, ...(c.headers || {}) };
  let payload = null;
  if (c.body !== undefined) { payload = Buffer.from(JSON.stringify(c.body)); headers['Content-Type'] = 'application/json'; }
  if (c.multipart) { const m = multipartBody(c.multipart); payload = m.body; headers['Content-Type'] = m.contentType; }
  if (payload) headers['Content-Length'] = payload.length;
  if (c.websocket) Object.assign(headers, { Connection: 'Upgrade', Upgrade: 'websocket', 'Sec-WebSocket-Version': '13', 'Sec-WebSocket-Key': Buffer.from('llmprobe-compat-1').toString('base64') });
  const transport = url.protocol === 'https:' ? https : http;
  return new Promise(resolve => {
    const req = transport.request(url, { method: c.method, headers }, res => {
      const chunks = [];
      res.on('data', d => chunks.push(d));
      res.on('end', () => resolve(finish(res.statusCode, res.headers, Buffer.concat(chunks))));
      res.on('error', e => resolve(finish(0, {}, Buffer.from(`response error: ${e.message}`))));
    });
    req.on('upgrade', (res, socket) => { socket.destroy(); resolve(finish(101, res.headers, Buffer.alloc(0))); });
    req.on('error', e => resolve(finish(0, {}, Buffer.from(`request error: ${e.message}`))));
    req.setTimeout(TIMEOUT_MS, () => req.destroy(new Error('timeout')));
    req.end(payload ?? undefined);
  });
  function finish(status, resHeaders, raw) {
    const text = raw.toString('utf8');
    let json;
    try { json = JSON.parse(text); } catch { /* not JSON */ }
    return { url: url.toString(), requestHeaders: headers, payload, status, headers: resHeaders, raw, text, json };
  }
}

// ---------- non-standard field notes (keys only, never values) ----------
const STANDARD = {
  top: ['id', 'object', 'created', 'model', 'choices', 'usage', 'system_fingerprint', 'service_tier'],
  choice: ['index', 'message', 'finish_reason', 'logprobs'],
  message: ['role', 'content', 'refusal', 'tool_calls', 'annotations', 'audio', 'function_call'],
  usage: ['prompt_tokens', 'completion_tokens', 'total_tokens', 'prompt_tokens_details', 'completion_tokens_details'],
};
function extraFields(v) {
  if (!chatShape(v)) return [];
  const diff = (o, std, prefix) => Object.keys(o || {}).filter(k => !std.includes(k)).map(k => `${prefix}${k}`);
  return [...diff(v, STANDARD.top, ''), ...diff(v.choices[0], STANDARD.choice, 'choices[].'), ...diff(v.choices[0].message, STANDARD.message, 'choices[].message.'), ...diff(v.usage, STANDARD.usage, 'usage.')];
}

// ---------- main ----------
async function main() {
  const [name, runDir] = process.argv.slice(2);
  if (!name || !runDir) throw new Error('usage: node endpoints.cjs <aimanager|foundry> <run-dir>');
  const p = provider(name);
  const redact = redactor(p.key);
  const write = writer(path.join(runDir, 'raw', name), redact);
  const record = (id, r) => {
    const body = r.payload ? (r.requestHeaders['Content-Type'] === 'application/json' ? JSON.parse(r.payload.toString()) : `<multipart, ${r.payload.length} bytes>`) : null;
    write(`${id}.request.json`, JSON.stringify({ url: r.url, headers: redactHeaders(r.requestHeaders), body }, null, 2) + '\n');
    write(`${id}.response.headers.json`, JSON.stringify({ status: r.status, headers: redactHeaders(r.headers) }, null, 2) + '\n');
    write(`${id}.response.body`, r.raw);
  };

  // CASE_FILTER=id1,id2 re-runs only those cases and merges them into an existing result file.
  const only = process.env.CASE_FILTER ? new Set(process.env.CASE_FILTER.split(',').map(x => x.trim())) : null;
  const resultFile = path.join(runDir, `endpoints-${name}.json`);
  const previous = only && fs.existsSync(resultFile) ? JSON.parse(fs.readFileSync(resultFile, 'utf8')) : null;
  const cases = buildCases(p.model).filter(c => !only || only.has(c.id));

  const unknownRoute = {};
  for (const method of new Set(cases.filter(c => c.routeOnly).map(c => c.method))) {
    const r = await send(p, { method, path: UNKNOWN_ROUTE });
    record(`unknown-route-${method}`, r);
    unknownRoute[method] = r;
    console.log(`${name.padEnd(9)} unknown-route ${method}: ${errorNote(r)}`);
  }

  const results = [];
  for (const c of cases) {
    const r = await send(p, c);
    record(c.id, r);
    const ok2xx = r.status >= 200 && r.status < 300;
    let verdict;
    if (c.routeOnly) verdict = c.check(r, unknownRoute[c.method]);
    else if (c.websocket) verdict = c.check(r);
    else if (!ok2xx) verdict = fail(errorNote(r));
    else verdict = c.check(r);
    const result = {
      id: c.id, group: c.group, endpoint: c.endpoint, method: c.method, path: c.path, url: r.url, status: r.status,
      supported: verdict.ok, note: verdict.note,
      routeOnly: !!c.routeOnly, deploymentDependent: !!c.deploymentDependent, extension: !!c.extension,
    };
    if (c.compat && verdict.ok) result.extraFields = extraFields(r.json);
    results.push(result);
    console.log(`${name.padEnd(9)} ${verdict.ok ? 'SUPPORTED    ' : 'NOT SUPPORTED'} ${String(r.status).padStart(3)} ${c.id}${verdict.note ? ` — ${verdict.note.slice(0, 110)}` : ''}`);
  }

  const compat = [];
  for (const c of compatCases(p.model).filter(c => !only || only.has(c.id))) {
    const r = await send(p, c);
    record(c.id, r);
    const ok = r.status >= 200 && r.status < 300 && chatShape(r.json);
    compat.push({ id: c.id, note: c.note, status: r.status, accepted: ok, detail: ok ? 'accepted' : errorNote(r) });
    console.log(`${name.padEnd(9)} compat ${c.id}: ${r.status} ${ok ? 'accepted' : 'rejected'}`);
  }

  const merge = (old = [], fresh) => old.map(o => fresh.find(f => f.id === o.id) ?? o).concat(fresh.filter(f => !old.some(o => o.id === f.id)));
  const out = previous
    ? { ...previous, results: merge(previous.results, results), compat: merge(previous.compat, compat) }
    : { provider: name, label: p.label, base: p.base, model: p.model, results, compat };
  out.unknownRoute = { ...(previous?.unknownRoute || {}), ...Object.fromEntries(Object.entries(unknownRoute).map(([m, r]) => [m, errorNote(r)])) };
  fs.writeFileSync(resultFile, redact(JSON.stringify(out, null, 2)) + '\n');
}

module.exports = { chatStreamCheck, errorNote, buildCases };
if (require.main === module) main().catch(e => { console.error(e.message); process.exitCode = 1; });
