// Installs the packed tarball into an empty project and imports it both ways —
// run from inside sdk/browser or sdk/server, after `npm pack`.
//
//   node ../scripts/verify-consumer-install.mjs agentray-browser-0.1.0.tgz
//
// Everything else in CI imports the SDK by relative path from inside its own
// repo, where the `exports` map, the `files` list and the type declarations are
// never consulted. This step is the closest thing to being the first user: a
// broken exports condition, a dist file left out of `files`, or a .d.ts that
// points at nothing all survive the unit tests and die here.
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const tarball = process.argv[2];
if (!tarball) {
  console.error('usage: verify-consumer-install.mjs <tarball.tgz>');
  process.exit(2);
}

const pkg = JSON.parse(readFileSync('package.json', 'utf8'));
const tarballPath = resolve(tarball);

// The one symbol each package's documented first line of code reaches for.
const ENTRY_SYMBOL = {
  '@agentray/browser': 'init',
  '@agentray/server': 'AgentRayServerClient',
};
const symbol = ENTRY_SYMBOL[pkg.name];
if (!symbol) {
  console.error(`no documented entry symbol recorded for ${pkg.name} — add one to verify-consumer-install.mjs`);
  process.exit(2);
}

// The money taxonomy is a wire contract, so its values — not just its names —
// have to survive the build. A published 0.2.0 bundle that ships
// `REVENUE_EVENT = 'revenue_event'` would file every customer's bookings under
// an event name the Overview read never queries, and nothing else in CI would
// notice.
const MONEY_CONSTANTS = {
  '@agentray/browser': {
    REVENUE_EVENT: 'revenue',
    REVENUE_REVERSED_EVENT: 'revenue_reversed',
    REFUND_KIND: 'refund',
  },
  '@agentray/server': {
    REVENUE_EVENT: 'revenue',
    REVENUE_REVERSED_EVENT: 'revenue_reversed',
    REFUND_KIND: 'refund',
  },
};
const constants = MONEY_CONSTANTS[pkg.name];
if (!constants) {
  console.error(`no money constants recorded for ${pkg.name} — add them to verify-consumer-install.mjs`);
  process.exit(2);
}
const constantChecks = Object.entries(constants)
  .map(([name, value]) =>
    `if (sdk.${name} !== ${JSON.stringify(value)}) throw new Error('${name} should be ${value}, got ' + String(sdk.${name}));`)
  .join('\n   ');

const dir = mkdtempSync(join(tmpdir(), 'agentray-consumer-'));
const run = (cmd, args) => execFileSync(cmd, args, { cwd: dir, stdio: 'inherit' });

writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'consumer', private: true, version: '1.0.0' }));
run('npm', ['install', '--no-audit', '--no-fund', tarballPath]);

// ESM and CJS are separate build outputs reached through separate `exports`
// conditions, so a break in one is invisible from the other.
run('node', ['--input-type=module', '-e',
  `import * as sdk from '${pkg.name}';
   if (typeof sdk.${symbol} !== 'function') throw new Error('${symbol} missing from the esm entrypoint');
   ${constantChecks}
   console.log('  esm ok: ${symbol} + ${Object.keys(constants).length} money constants');`]);

run('node', ['--input-type=commonjs', '-e',
  `const sdk = require('${pkg.name}');
   if (typeof sdk.${symbol} !== 'function') throw new Error('${symbol} missing from the cjs entrypoint');
   ${constantChecks}
   console.log('  cjs ok: ${symbol} + ${Object.keys(constants).length} money constants');`]);

// The declaration file is what an editor resolves; a package whose types 404
// installs fine and is miserable to use.
const types = join(dir, 'node_modules', pkg.name, (pkg.types ?? '').replace(/^\.\//, ''));
const dts = readFileSync(types, 'utf8');
for (const name of [symbol, ...Object.keys(constants)]) {
  if (!new RegExp(`\\b${name}\\b`).test(dts)) {
    console.error(`${pkg.types} does not declare ${name}`);
    process.exit(1);
  }
}
console.log(`  types ok: ${pkg.types} declares ${symbol} and the money constants`);

console.log(`${pkg.name}@${pkg.version} installs and imports from a clean project`);
