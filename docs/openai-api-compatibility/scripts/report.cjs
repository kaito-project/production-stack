'use strict';
// Builds report.md and results.json from the per-provider result files.
// Usage: node report.cjs <run-dir>
const fs = require('node:fs');
const path = require('node:path');

const PROVIDERS = ['aimanager', 'foundry'];
const runDir = process.argv[2];
if (!runDir) throw new Error('usage: node report.cjs <run-dir>');
const read = f => JSON.parse(fs.readFileSync(path.join(runDir, f), 'utf8'));

const data = Object.fromEntries(PROVIDERS.map(p => {
  const endpoints = read(`endpoints-${p}.json`);
  const llmprobe = read(`llmprobe-${p}.json`);
  return [p, { ...endpoints, llmprobe, results: [...endpoints.results, ...llmprobe.results] }];
}));
const labels = Object.fromEntries(PROVIDERS.map(p => [p, data[p].label]));

// Everything this script writes is meant to be shareable, so the providers'
// hostnames (and the Foundry resource name, which also appears in some error
// messages) are replaced with placeholders.
const MASKS = PROVIDERS.flatMap(p => {
  const host = (data[p].base.match(/^[a-z]+:\/\/([^/]+)/i) || [])[1] || '';
  if (!host || host.startsWith('<')) return []; // already masked
  const name = host.split('.')[0];
  return [[host, `<${p}-endpoint>`], ...(p === 'foundry' ? [[name, '<foundry-account>']] : [])];
});
const mask = text => MASKS.reduce((out, [from, to]) => out.split(from).join(to), text);

// A deployment-dependent case that failed only because the deployed model
// lacks the capability still counts as Supported by the platform, marked
// "needs a suitable model". Platform-level rejections stay Not supported:
// AI Manager's BBR fallback / native 400 (no x-kaito-error-source: inferenceset),
// EPP's "no parser registered" (the gateway does not route the API), or
// Foundry's unknown-route "Resource not found".
const MODEL_GATED = {
  aimanager: r => /x-kaito-error-source: inferenceset/.test(r.note) && !/no parser registered/.test(r.note),
  foundry: r => /operation does not work with the specified model|Model not supported|API deployment for this resource does not exist/i.test(r.note),
};
for (const p of PROVIDERS) {
  for (const r of data[p].results) {
    if (r.deploymentDependent && !r.supported && r.status >= 400 && r.status < 500 && MODEL_GATED[p](r)) {
      r.supported = true;
      r.needsModel = true;
      r.note = `platform supports this API; the deployed model does not (${r.note})`;
    }
  }
}

const ORDER = ['A.', 'B.', 'C.', 'D.', 'E.', 'F.', 'G.', 'H.', 'I.', 'J.'];
const groupRank = g => ORDER.findIndex(prefix => g.startsWith(prefix));
// chat-vision shares its path with chat; label it so the endpoint summary keeps them apart.
for (const p of PROVIDERS) for (const r of data[p].results) if (r.id === 'chat-vision') r.endpoint = `${r.method} /v1/chat/completions (image input)`;
const ids = [...new Map(data.aimanager.results.map(r => [r.id, r])).values()]
  .sort((a, b) => groupRank(a.group) - groupRank(b.group));
const byId = p => Object.fromEntries(data[p].results.map(r => [r.id, r]));
const res = Object.fromEntries(PROVIDERS.map(p => [p, byId(p)]));

const mark = r => r ? (r.supported ? (r.needsModel ? '✅ Supported (needs model)' : '✅ Supported') : '❌ Not supported') : '—';
const esc = s => String(s ?? '').replace(/\|/g, '\\|').replace(/\n/g, ' ');
const tags = r => [r.routeOnly && 'route only', r.deploymentDependent && 'depends on deployment', r.extension && 'not OpenAI API'].filter(Boolean);
const display = r => `\`${r.method} ${(r.endpoint || '').replace(/^[A-Z]+ /, '')}\``;

// endpoint summary: one row per (group, endpoint); its base case is the first case
const endpoints = [];
for (const r of ids) {
  const key = `${r.group}|${r.endpoint}`;
  if (!endpoints.some(e => e.key === key)) endpoints.push({ key, group: r.group, endpoint: r.endpoint, base: r.id, cases: [] });
  endpoints.find(e => e.key === key).cases.push(r.id);
}

const count = p => ({ total: ids.length, supported: ids.filter(r => res[p][r.id]?.supported).length });
const endpointCount = p => endpoints.filter(e => res[p][e.base]?.supported).length;

const lines = [];
const L = s => lines.push(s ?? '');
const today = path.basename(path.resolve(runDir));

L(`# OpenAI API compatibility: ${labels.aimanager} vs ${labels.foundry}`);
L();
L(`Run: ${today.replace(/^(\d{4})(\d{2})(\d{2})$/, '$1-$2-$3')}. This report compares **OpenAI API support only**. It does not compare performance, serving behaviour or model quality: the two services run different models, so generated content is never judged, and no timing or throughput figures are reported.`);
L();
L('## 1. Environment');
L();
L(`| | ${labels.aimanager} | ${labels.foundry} |`);
L('| --- | --- | --- |');
L(`| Base URL | \`${data.aimanager.base}\` | \`${data.foundry.base}\` |`);
L(`| Deployment (\`model\`) | \`${data.aimanager.model}\` | \`${data.foundry.model}\` |`);
L('| Authentication | `Authorization: Bearer <key>` | `api-key: <key>` |');
L(`| Unknown-route reply (GET) | ${esc(data.aimanager.unknownRoute?.GET)} | ${esc(data.foundry.unknownRoute?.GET)} |`);
L(`| llmprobe | ${data.aimanager.llmprobe.version} | ${data.foundry.llmprobe.version} |`);
L();
L('## 2. Method');
L();
L(`- **${ids.length} test cases per service, each run once** (${ids.length - 2} HTTP cases plus 2 llmprobe probes), across ${endpoints.length} endpoints in groups A–J. Paths are written as OpenAI paths; \`/v1/...\` maps onto each base URL (\`/openai/v1/...\` on Foundry) and vLLM root paths (\`/health\`, \`/tokenize\`, …) stay at the host root.`);
L('- **Supported** = HTTP 2xx and the response has the protocol shape of that API (e.g. `chat.completion` with `choices[].message`, `finish_reason` and `usage`; SSE `chat.completion.chunk` events ending with `data: [DONE]`; `choices` length 2 for `n=2`; a `logprobs` object; `object: list` with `data[]`). **Not supported** = anything else; the Notes give the HTTP status and the error. What the model generated (content, JSON validity, which tool was called) is not judged.');
L('- **Route only** cases call a list endpoint or a nonexistent ID (`probe-nonexistent`), because no resources are created. They are Supported when the reply is a resource-level not-found / invalid-ID error, i.e. it names the probed ID or differs from the service\'s reply to a path that does not exist (Section 1). They show that the route exists, not that the full resource lifecycle works.');
L('- **Depends on deployment** marks endpoints whose support depends on the deployed model type (embeddings, images, audio, moderations, vision, realtime, score/rerank/pooling). Both deployments are chat models, so a Not supported there is not a platform gap by itself.');
L('- **Supported (needs model)**: for deployment-dependent endpoints, an error caused only by the deployed model lacking the capability still counts as Supported, because the platform serves the API once a suitable model is deployed. This applies when the request reached the model server and was rejected for the model (AI Manager: an error from vLLM, `x-kaito-error-source: inferenceset`; Foundry: an error naming the model or deployment). Platform-level rejections stay Not supported: AI Manager\'s gateway fallback or BBR native `400`, EPP\'s `no parser registered` (the gateway does not route the API), and Foundry\'s unknown-route `Resource not found`.');
L('- **Not OpenAI API** marks vLLM extensions and runtime paths (group J), kept for completeness. The Anthropic `/v1/messages` paths are excluded.');
L('- **Portable request profile**: every request is identical across services except for the URL and the auth header. Chat uses `max_completion_tokens: 512` and no `temperature`; Responses uses `store: false`; legacy Completions uses `max_tokens` (the only field that API defines). Nothing is created in either account.');
L('- **llmprobe** (ordinary SSE and JSON SSE, 1 round, concurrency 1) runs through a loopback adapter that applies the provider URL and credential and rewrites llmprobe\'s `max_tokens` to `max_completion_tokens`; responses are streamed back unchanged. Its `validate_json` result is shown in Notes only.');
L();
L('## 3. Summary');
L();
L(`| | ${labels.aimanager} | ${labels.foundry} |`);
L('| --- | --- | --- |');
L(`| Endpoints supported | ${endpointCount('aimanager')} / ${endpoints.length} | ${endpointCount('foundry')} / ${endpoints.length} |`);
L();
L('An endpoint (method + path) is Supported when its base (first) test case is Supported, so an endpoint can be Supported while some of its other test cases fail. Section 4 has the result of every test case.');
L();
const bucket = (a, f) => endpoints.filter(e => !!res.aimanager[e.base]?.supported === a && !!res.foundry[e.base]?.supported === f);
// Renders endpoints as a nested list grouped by API family, tagging route-only
// and deployment-dependent rows so the summary reads without the table.
const listByGroup = rows => {
  const out = [];
  for (const group of [...new Set(rows.map(e => e.group))]) {
    out.push(`  - ${group.replace(/^[A-J]\.\s*/, '')}`);
    for (const e of rows.filter(x => x.group === group)) {
      const t = tags(res.aimanager[e.base]).filter(x => x !== 'depends on deployment');
      const nm = PROVIDERS.filter(p => res[p][e.base]?.needsModel).map(p => labels[p]);
      if (nm.length) t.push(`needs a suitable model on ${nm.join(' and ')}`);
      else if (res.aimanager[e.base]?.deploymentDependent) t.push('depends on deployment');
      out.push(`    - \`${e.endpoint}\`${t.length ? ` — *${t.join(', ')}*` : ''}`);
    }
  }
  return out;
};
const section = (title, rows) => {
  L(`- **${title} (${rows.length})**${rows.length ? '' : ': none'}`);
  listByGroup(rows).forEach(l => L(l));
};
section('Supported by both', bucket(true, true));
section(`Only ${labels.aimanager}`, bucket(true, false));
section(`Only ${labels.foundry}`, bucket(false, true));
section('Neither', bucket(false, false));
L();
L('Endpoint summary. An endpoint is Supported when its base (first) test case is Supported; Section 4 has every case.');
L();
L(`| Group | Endpoint | Base case | ${labels.aimanager} | ${labels.foundry} | Tags |`);
L('| --- | --- | --- | --- | --- | --- |');
for (const e of endpoints) {
  const r = res.aimanager[e.base];
  L(`| ${e.group.replace(/^([A-J])\..*/, '$1')} | \`${esc(e.endpoint)}\` | \`${e.base}\`${e.cases.length > 1 ? ` (+${e.cases.length - 1})` : ''} | ${mark(res.aimanager[e.base])} | ${mark(res.foundry[e.base])} | ${tags(r).join(', ')} |`);
}
L();
L('## 4. Results by test case');
for (const group of ORDER) {
  const rows = ids.filter(r => r.group.startsWith(group));
  if (!rows.length) continue;
  L();
  L(`### ${rows[0].group}`);
  L();
  L(`| Case | Request | ${labels.aimanager} | ${labels.foundry} | Notes |`);
  L('| --- | --- | --- | --- | --- |');
  for (const r of rows) {
    const a = res.aimanager[r.id], f = res.foundry[r.id];
    const note = x => (x.note || '').replace(new RegExp(`^${x.status}: `), '');
    const notes = [
      tags(r).length ? `*${tags(r).join(', ')}*` : '',
      a ? `**AIM ${a.status || 'no response'}**${note(a) ? `: ${note(a)}` : ''}` : '',
      f ? `**Foundry ${f.status || 'no response'}**${note(f) ? `: ${note(f)}` : ''}` : '',
    ].filter(Boolean).join('<br>');
    L(`| \`${r.id}\` | ${display(r)} | ${mark(a)} | ${mark(f)} | ${esc(notes)} |`);
  }
}
L();
L('## 5. Compatibility notes');
L();
L('Parameter and field differences found during the run. They do not change the classification above.');
L();
L(`| Check | ${labels.aimanager} | ${labels.foundry} |`);
L('| --- | --- | --- |');
const compatIds = [...new Set(PROVIDERS.flatMap(p => (data[p].compat || []).map(c => c.id)))];
for (const id of compatIds) {
  const c = p => (data[p].compat || []).find(x => x.id === id);
  const cell = x => x ? (x.accepted ? `Accepted (${x.status})` : `Rejected — ${esc(x.detail)}`) : '—';
  L(`| ${esc(c('aimanager')?.note || c('foundry')?.note)} | ${cell(c('aimanager'))} | ${cell(c('foundry'))} |`);
}
const rewrites = p => data[p].llmprobe.rewrites?.length ? data[p].llmprobe.rewrites.join(', ') : 'none';
L(`| llmprobe adapter rewrites applied | ${rewrites('aimanager')} | ${rewrites('foundry')} |`);
const extra = p => {
  const chat = res[p].chat;
  return chat?.extraFields?.length ? chat.extraFields.map(f => `\`${f}\``).join(', ') : 'none';
};
L(`| Non-standard fields in a \`chat.completion\` response (keys only) | ${extra('aimanager')} | ${extra('foundry')} |`);
const sseExtra = p => {
  const n = res[p]['chat-stream']?.note || '';
  return n.includes('non-standard') ? esc(n) : 'none';
};
L(`| Non-standard SSE chunks in a chat stream | ${sseExtra('aimanager')} | ${sseExtra('foundry')} |`);
const jsonValidate = p => {
  const l = res[p]['llmprobe-json-sse']?.llmprobe;
  return l ? `llmprobe status ${l.status}${l.error ? ` (${esc(l.error)})` : ''}` : '—';
};
L(`| llmprobe \`validate_json\` (model behaviour, not classified) | ${jsonValidate('aimanager')} | ${jsonValidate('foundry')} |`);
L();
const notesFile = path.join(runDir, 'notes.md');
let evidence = 6;
if (fs.existsSync(notesFile)) {
  L('## 6. Run notes');
  L();
  L(fs.readFileSync(notesFile, 'utf8').trim());
  L();
  evidence = 7;
}
L(`## ${evidence}. Evidence and reproduction`);
L();
L(`- Per-case evidence (requests, response headers and bodies, llmprobe output) is written by the scripts to \`raw/\` and the per-provider result files next to this report. It stays local and is not committed; see \`../.gitignore\`. Credentials are redacted in every file.`);
L('- Machine-readable results: `results.json`.');
L('- Reproduce: see `../README.md`.');
L();

fs.writeFileSync(path.join(runDir, 'report.md'), mask(lines.join('\n')));
fs.writeFileSync(path.join(runDir, 'results.json'), mask(JSON.stringify({
  run: today,
  providers: Object.fromEntries(PROVIDERS.map(p => [p, { label: labels[p], base: data[p].base, model: data[p].model, unknownRoute: data[p].unknownRoute, llmprobe: { version: data[p].llmprobe.version, rewrites: data[p].llmprobe.rewrites } }])),
  summary: Object.fromEntries(PROVIDERS.map(p => [p, { cases: count(p), endpoints: { total: endpoints.length, supported: endpointCount(p) } }])),
  cases: ids.map(r => ({ id: r.id, group: r.group, endpoint: r.endpoint, routeOnly: r.routeOnly, deploymentDependent: r.deploymentDependent, extension: r.extension, ...Object.fromEntries(PROVIDERS.map(p => [p, { supported: res[p][r.id]?.supported ?? null, needsModel: !!res[p][r.id]?.needsModel, status: res[p][r.id]?.status ?? null, note: res[p][r.id]?.note ?? '' }])) })),
  compat: Object.fromEntries(PROVIDERS.map(p => [p, data[p].compat])),
}, null, 2)) + '\n');
console.log(`wrote ${path.join(runDir, 'report.md')} and results.json`);
