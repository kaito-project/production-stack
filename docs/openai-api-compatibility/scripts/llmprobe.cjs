'use strict';
// Runs llmprobe once (ordinary SSE + JSON SSE) against one provider through a
// loopback adapter that applies the provider's URL and authentication and
// normalizes the request to the portable profile (max_completion_tokens, no
// temperature). Responses are streamed back unchanged. Only API support is
// recorded; llmprobe's timing figures stay in its raw output.
//
// Usage: node llmprobe.cjs <aimanager|foundry> <run-dir>
// LLMPROBE_BIN overrides the llmprobe binary (default: llmprobe on PATH).
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const https = require('node:https');
const { spawn, execFileSync } = require('node:child_process');
const { provider, redactor, writer, redactHeaders } = require('./lib.cjs');
const { chatStreamCheck, errorNote } = require('./endpoints.cjs');

const BINARY = process.env.LLMPROBE_BIN || 'llmprobe';
const PROBES = {
  'llmprobe-chat-sse': { label: 'chat-sse', prompt: 'Write a concise explanation of how rain forms, in about 100 words.' },
  'llmprobe-json-sse': { label: 'json-sse', prompt: 'Return only a JSON object with city Paris, country France, and an array named landmarks containing three famous landmarks.', json: true },
};

function normalize(body) {
  const out = { ...body };
  const rewritten = [];
  if ('max_tokens' in out) { out.max_completion_tokens ??= out.max_tokens; delete out.max_tokens; rewritten.push('max_tokens → max_completion_tokens'); }
  if ('temperature' in out) { delete out.temperature; rewritten.push('temperature removed'); }
  return { body: out, rewritten };
}

async function main() {
  const [name, runDir] = process.argv.slice(2);
  if (!name || !runDir) throw new Error('usage: node llmprobe.cjs <aimanager|foundry> <run-dir>');
  const p = provider(name);
  const redact = redactor(p.key);
  const write = writer(path.join(runDir, 'raw', name, 'llmprobe'), redact);
  const captured = {};
  const rewrites = new Set();

  const server = http.createServer((incoming, outgoing) => {
    // base_url carries the probe label as a path prefix: /<label>/v1/chat/completions
    const label = (incoming.url.match(/^\/([a-z-]+)\//) || [])[1] || 'unknown';
    const buffers = [];
    incoming.on('data', d => buffers.push(d));
    incoming.on('end', () => {
      let parsed;
      try { parsed = JSON.parse(Buffer.concat(buffers).toString()); } catch { outgoing.writeHead(400).end(); return; }
      const { body, rewritten } = normalize(parsed);
      rewritten.forEach(r => rewrites.add(r));
      const payload = Buffer.from(JSON.stringify(body));
      const url = p.url('/v1/chat/completions');
      const headers = { ...p.headers, 'Content-Type': 'application/json', 'Content-Length': payload.length };
      write(`${label}.request.json`, JSON.stringify({ url: url.toString(), headers: redactHeaders(headers), original: parsed, sent: body }, null, 2) + '\n');
      const transport = url.protocol === 'https:' ? https : http;
      const upstream = transport.request(url, { method: 'POST', headers }, res => {
        const chunks = [];
        outgoing.writeHead(res.statusCode, res.headers);
        res.on('data', d => { chunks.push(d); outgoing.write(d); });
        res.on('end', () => {
          const raw = Buffer.concat(chunks);
          write(`${label}.response.headers.json`, JSON.stringify({ status: res.statusCode, headers: redactHeaders(res.headers) }, null, 2) + '\n');
          write(`${label}.response.body`, raw);
          captured[label] = { status: res.statusCode, headers: res.headers, raw, text: raw.toString('utf8') };
          try { captured[label].json = JSON.parse(captured[label].text); } catch { /* SSE */ }
          outgoing.end();
        });
      });
      upstream.on('error', e => { captured[label] = { status: 0, headers: {}, raw: Buffer.alloc(0), text: `request error: ${e.message}` }; if (!outgoing.headersSent) outgoing.writeHead(502); outgoing.end(); });
      upstream.setTimeout(180_000, () => upstream.destroy(new Error('timeout')));
      upstream.end(payload);
    });
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const port = server.address().port;

  const config = {
    defaults: { max_tokens: 512, timeout: '180s', concurrency: 1 },
    providers: Object.values(PROBES).map(probe => ({
      name: 'openai', label: probe.label, api_key: '${PROBE_KEY}', base_url: `http://127.0.0.1:${port}/${probe.label}`,
      models: [{ name: p.model, prompt: probe.prompt, ...(probe.json ? { response_format: 'json', validate_json: true } : {}) }],
    })),
  };
  const configPath = path.join(runDir, 'raw', name, 'llmprobe', 'probes.json');
  write('probes.json', JSON.stringify(config, null, 2) + '\n');

  const output = await new Promise((resolve, reject) => {
    // The adapter injects the real credential; llmprobe only sees a placeholder.
    const child = spawn(BINARY, ['probe', '-c', configPath, '-f', 'json', '--fail-on', 'none'], { env: { ...process.env, PROBE_KEY: 'adapter-injects-credential', NO_PROXY: '127.0.0.1,localhost', no_proxy: '127.0.0.1,localhost' } });
    const out = [], err = [];
    child.stdout.on('data', d => out.push(d));
    child.stderr.on('data', d => err.push(d));
    child.on('error', reject);
    child.on('close', code => {
      write('llmprobe.stdout.json', Buffer.concat(out));
      write('llmprobe.stderr.txt', Buffer.concat(err));
      code === 0 ? resolve(Buffer.concat(out).toString()) : reject(new Error(`llmprobe exited ${code}; see llmprobe.stderr.txt`));
    });
  });
  server.close();
  server.closeAllConnections();

  const records = JSON.parse(output);
  const version = execFileSync(BINARY, ['version'], { encoding: 'utf8' }).trim();
  const results = Object.entries(PROBES).map(([id, probe]) => {
    const record = records.find(r => r.provider === probe.label) || {};
    const r = captured[probe.label];
    const llmprobeNote = `llmprobe status ${record.status ?? 'n/a'}${record.error ? ` (${record.error})` : ''}`;
    let verdict;
    if (!r) verdict = { ok: false, note: 'llmprobe sent no request' };
    else if (r.status < 200 || r.status >= 300) verdict = { ok: false, note: errorNote(r) };
    else verdict = chatStreamCheck(r);
    return {
      id, group: 'C. llmprobe', endpoint: 'POST /v1/chat/completions (llmprobe)', method: 'POST', path: '/v1/chat/completions', status: r?.status ?? 0,
      supported: verdict.ok, note: [verdict.note, llmprobeNote].filter(Boolean).join('; '), llmprobe: { status: record.status ?? null, error: record.error ?? null },
      routeOnly: false, deploymentDependent: false, extension: false,
    };
  });
  for (const r of results) console.log(`${name.padEnd(9)} ${r.supported ? 'SUPPORTED    ' : 'NOT SUPPORTED'} ${String(r.status).padStart(3)} ${r.id} — ${r.note}`);
  fs.writeFileSync(path.join(runDir, `llmprobe-${name}.json`), redact(JSON.stringify({ provider: name, version, rewrites: [...rewrites], results }, null, 2)) + '\n');
}

main().catch(e => { console.error(e.message); process.exitCode = 1; });
