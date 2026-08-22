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

const dir = mkdtempSync(join(tmpdir(), 'agentray-consumer-'));
const run = (cmd, args) => execFileSync(cmd, args, { cwd: dir, stdio: 'inherit' });

writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'consumer', private: true, version: '1.0.0' }));
run('npm', ['install', '--no-audit', '--no-fund', tarballPath]);

// ESM and CJS are separate build outputs reached through separate `exports`
// conditions, so a break in one is invisible from the other.
run('node', ['--input-type=module', '-e',
  `import * as sdk from '${pkg.name}';
   if (typeof sdk.${symbol} !== 'function') throw new Error('${symbol} missing from the esm entrypoint');
   console.log('  esm ok: ${symbol}');`]);

run('node', ['--input-type=commonjs', '-e',
  `const sdk = require('${pkg.name}');
   if (typeof sdk.${symbol} !== 'function') throw new Error('${symbol} missing from the cjs entrypoint');
   console.log('  cjs ok: ${symbol}');`]);

// The declaration file is what an editor resolves; a package whose types 404
// installs fine and is miserable to use.
const types = join(dir, 'node_modules', pkg.name, (pkg.types ?? '').replace(/^\.\//, ''));
const dts = readFileSync(types, 'utf8');
if (!new RegExp(`\\b${symbol}\\b`).test(dts)) {
  console.error(`${pkg.types} does not declare ${symbol}`);
  process.exit(1);
}
console.log(`  types ok: ${pkg.types} declares ${symbol}`);

console.log(`${pkg.name}@${pkg.version} installs and imports from a clean project`);
