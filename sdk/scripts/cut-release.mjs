// Bumps one SDK's manifest, commits it, and creates the release tag.
//
//   node sdk/scripts/cut-release.mjs browser patch
//   node sdk/scripts/cut-release.mjs python 0.2.0
//
// It deliberately does NOT push. Pushing the tag is what triggers publication
// (.github/workflows/sdk-release.yml), so it stays a separate, deliberate act;
// the command to run is printed at the end.
//
// `npm version` is not used for the tagging half because it writes a tag named
// `v<version>`, which collides between the two npm packages and with the
// product's own v-tags. Here the tag names the package: `browser-v0.2.0`.
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';

const PACKAGES = {
  browser: { dir: 'sdk/browser', kind: 'npm' },
  server: { dir: 'sdk/server', kind: 'npm' },
  python: { dir: 'sdk/python', kind: 'pypi' },
  swift: { dir: 'sdk/swift', kind: 'swiftpm' },
};

const [pkg, bump] = process.argv.slice(2);
if (!PACKAGES[pkg] || !bump) {
  console.error('usage: cut-release.mjs <browser|server|python|swift> <patch|minor|major|x.y.z>');
  process.exit(2);
}

const sh = (cmd, args, opts = {}) => execFileSync(cmd, args, { encoding: 'utf8', ...opts }).trim();
const { dir, kind } = PACKAGES[pkg];

if (sh('git', ['status', '--porcelain'])) {
  console.error('working tree is dirty — commit or stash before cutting a release');
  process.exit(1);
}

// --- current version -------------------------------------------------------
const manifest = kind === 'npm' ? `${dir}/package.json` : kind === 'pypi' ? `${dir}/pyproject.toml` : null;
let current;
if (kind === 'npm') {
  current = JSON.parse(readFileSync(manifest, 'utf8')).version;
} else if (kind === 'pypi') {
  current = /^\s*version\s*=\s*["']([^"']+)["']/m.exec(readFileSync(manifest, 'utf8'))[1];
} else {
  // SwiftPM has no version in the manifest — git tags are the version list.
  const tags = sh('git', ['tag', '--list', 'swift-v*']).split('\n').filter(Boolean);
  current = tags.map((t) => t.replace('swift-v', '')).sort(cmpSemver).pop() ?? '0.0.0';
}

function cmpSemver(a, b) {
  const pa = a.split('.').map(Number), pb = b.split('.').map(Number);
  return pa[0] - pb[0] || pa[1] - pb[1] || pa[2] - pb[2];
}

// --- next version ----------------------------------------------------------
let next;
if (/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(bump)) {
  next = bump;
} else {
  const [maj, min, pat] = current.replace(/-.*$/, '').split('.').map(Number);
  next = { major: `${maj + 1}.0.0`, minor: `${maj}.${min + 1}.0`, patch: `${maj}.${min}.${pat + 1}` }[bump];
  if (!next) {
    console.error(`unknown bump "${bump}" — use patch, minor, major, or an explicit x.y.z`);
    process.exit(2);
  }
}

const tag = `${pkg}-v${next}`;
if (sh('git', ['tag', '--list', tag])) {
  console.error(`tag ${tag} already exists — a published version is not re-cut, bump again`);
  process.exit(1);
}

// --- write, verify, commit, tag -------------------------------------------
console.log(`${pkg}: ${current} -> ${next}`);

if (kind === 'npm') {
  const raw = readFileSync(manifest, 'utf8');
  writeFileSync(manifest, raw.replace(/("version":\s*)"[^"]+"/, `$1"${next}"`));
  // Keep the lockfile's own version field in step; npm rewrites it on install
  // anyway, and a lockfile that disagrees with its manifest is noise in a diff.
  execFileSync('npm', ['install', '--package-lock-only', '--no-audit', '--no-fund'], { cwd: dir, stdio: 'inherit' });
} else if (kind === 'pypi') {
  const raw = readFileSync(manifest, 'utf8');
  writeFileSync(manifest, raw.replace(/^(\s*version\s*=\s*)["'][^"']+["']/m, `$1"${next}"`));
}

const changed = kind === 'swiftpm' ? [] : sh('git', ['diff', '--name-only']).split('\n').filter(Boolean);
if (changed.length) {
  execFileSync('git', ['add', ...changed], { stdio: 'inherit' });
  execFileSync('git', ['commit', '-m', `release: ${pkg} SDK ${next}`], { stdio: 'inherit' });
}

// Resolve the tag against the tree it will actually build, before creating it.
execFileSync('node', ['sdk/scripts/resolve-tag.mjs', tag], { stdio: 'inherit' });

execFileSync('git', ['tag', '-a', tag, '-m', `${pkg} SDK ${next}`], { stdio: 'inherit' });

console.log(`
tagged ${tag}. Nothing is published yet.

  git push origin main ${tag}

That tag push runs .github/workflows/sdk-release.yml: it rebuilds and re-verifies
the tagged tree, creates the GitHub Release with the artefact attached, and then
publishes to the registry if the token for it is configured.
`);
