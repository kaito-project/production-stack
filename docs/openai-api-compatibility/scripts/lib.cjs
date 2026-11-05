'use strict';
// Shared provider settings for the OpenAI API compatibility comparison.
// Credentials are read from the environment only and redacted from every
// file this suite writes.
const fs = require('node:fs');
const path = require('node:path');

const PROVIDERS = {
  aimanager: { label: 'AI Manager (production-stack)', prefix: 'AIM', auth: key => ({ Authorization: `Bearer ${key}` }) },
  foundry: { label: 'Azure AI Foundry', prefix: 'FDY', auth: key => ({ 'api-key': key }) },
};

function provider(name) {
  const spec = PROVIDERS[name];
  if (!spec) throw new Error(`unknown provider ${name}; expected one of ${Object.keys(PROVIDERS).join(', ')}`);
  const env = suffix => {
    const value = process.env[`${spec.prefix}_${suffix}`];
    if (!value) throw new Error(`${spec.prefix}_${suffix} is not set`);
    return value;
  };
  const base = new URL(env('BASE').replace(/\/+$/, ''));
  const key = env('KEY');
  const model = env('MODEL');
  // Paths are written as the OpenAI paths (/v1/...) or vLLM root paths (/health).
  // /v1/... maps onto the provider's base path (e.g. /openai/v1 on Foundry);
  // root paths stay at the host root.
  const v1Prefix = base.pathname.replace(/\/+$/, '');
  const url = p => new URL(p.startsWith('/v1/') || p === '/v1' ? v1Prefix + p.slice(3) : p, base.origin);
  return { name, label: spec.label, base: base.toString(), origin: base.origin, v1Prefix, key, model, headers: spec.auth(key), url };
}

function redactor(...secrets) {
  const values = secrets.filter(Boolean);
  return text => values.reduce((out, secret) => out.split(secret).join('<redacted>'), text);
}

function writer(dir, redact) {
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  return (file, content) => {
    const target = path.join(dir, file);
    if (Buffer.isBuffer(content) && !isText(content)) fs.writeFileSync(target, content);
    else fs.writeFileSync(target, redact(Buffer.isBuffer(content) ? content.toString('utf8') : content));
    return target;
  };
}

function isText(buffer) {
  return !buffer.subarray(0, 512).includes(0);
}

const redactHeaders = headers => Object.fromEntries(Object.entries(headers).map(([k, v]) => [k, /^(authorization|api-key|x-api-key|cookie|set-cookie)$/i.test(k) ? '<redacted>' : v]));

module.exports = { PROVIDERS, provider, redactor, writer, redactHeaders };
