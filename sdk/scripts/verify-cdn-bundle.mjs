// Loads dist/index.global.js the way a <script> tag would and drives one real
// capture through it — run from inside sdk/browser (needs its jsdom devdep).
//
// The <script> path is what the copy-paste snippet in the product hands out,
// and it is the only build whose entrypoint is a global rather than an export.
// A bundler-config change can break it while esm and cjs stay green, so it gets
// its own executable check rather than a promise in a README.
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';

// This script lives in sdk/scripts/ but runs with sdk/browser/ as the cwd, and
// jsdom is that package's devDependency. A bare `import 'jsdom'` would resolve
// from the script's own directory and miss it.
const { JSDOM } = createRequire(`${process.cwd()}/`)('jsdom');

const dom = new JSDOM('<!doctype html><body></body>', {
  url: 'https://example.test/pricing',
  runScripts: 'dangerously',
});

const el = dom.window.document.createElement('script');
el.textContent = readFileSync('dist/index.global.js', 'utf8');
dom.window.document.head.appendChild(el);

if (typeof dom.window.AgentRay?.init !== 'function') {
  throw new Error('window.AgentRay.init is not a function');
}

const sent = [];
dom.window.fetch = async (_url, init) => {
  sent.push(JSON.parse(init.body));
  return { ok: true, status: 200 };
};

const ar = dom.window.AgentRay.init({ host: 'https://a.test', apiKey: 'k' });
ar.capture('user.pageview', { path: '/pricing' });
await ar.flush();

const ev = sent[0]?.batch?.[0];
if (!ev) throw new Error('flush sent no event');
if (ev.properties.platform !== 'web') {
  throw new Error(`expected platform web, got ${ev.properties.platform}`);
}

console.log('cdn bundle ok: global attached, capture reached the wire');
