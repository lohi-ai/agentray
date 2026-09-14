// Asserts that the tarball `npm publish` would upload actually contains the
// built entrypoints — run from inside sdk/browser or sdk/server.
//
// This is not ceremony. The Python wheel shipped for months able to build,
// upload and pass `twine check` while containing no package code at all,
// because nothing between `git push` and `pip install` looked inside the
// artefact. The same class of failure is one `files` or `tsup` edit away here,
// and `npm publish` is not undoable.
//
// The requirement list is derived from package.json rather than hardcoded, so
// adding a `unpkg`/`jsdelivr` entrypoint automatically becomes something the
// tarball must carry.
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';

const pkg = JSON.parse(readFileSync('package.json', 'utf8'));

const required = new Set(['LICENSE', 'README.md', 'CHANGELOG.md']);
const rel = (p) => (p ?? '').replace(/^\.\//, '');
for (const p of [pkg.main, pkg.module, pkg.types, pkg.unpkg, pkg.jsdelivr]) {
  if (p) required.add(rel(p));
}
for (const entry of Object.values(pkg.exports ?? {})) {
  if (typeof entry !== 'object') continue;
  for (const p of Object.values(entry)) if (typeof p === 'string') required.add(rel(p));
}

const packed = JSON.parse(execFileSync('npm', ['pack', '--dry-run', '--json'], { encoding: 'utf8' }));
const files = packed[0].files.map((f) => f.path);

const missing = [...required].filter((f) => !files.includes(f));
if (missing.length) {
  console.error(`${pkg.name}@${pkg.version} tarball is missing: ${missing.join(', ')}`);
  console.error(`present: ${files.join(', ')}`);
  console.error('see docs/RELEASING-SDK.md');
  process.exit(1);
}

// The changelog must actually describe the version being published — a bumped
// manifest with last release's entry ships a customer a migration note that
// does not mention the change they are installing, and the registry does not
// let you take the version back.
const changelog = readFileSync('CHANGELOG.md', 'utf8');
if (!changelog.includes(pkg.version)) {
  console.error(`CHANGELOG.md does not mention ${pkg.version}`);
  console.error('see the version policy in docs/RELEASING-SDK.md');
  process.exit(1);
}

console.log(`${pkg.name}@${pkg.version} tarball ok: ${files.length} files, ${required.size} required present`);
