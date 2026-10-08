// Bumps and commits one SDK's reviewed source version; main CI checks once,
// creates its package-specific tag and distributes the verified artifacts.
// This command never pushes and no longer creates a local release tag.
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';

const PACKAGES = {
  browser: { dir: 'sdk/browser', kind: 'npm' },
  server: { dir: 'sdk/server', kind: 'npm' },
  python: { dir: 'sdk/python', kind: 'pypi' },
  // The Swift SDK lives in lohi-ai/agentray-swift and is released by tagging
  // bare semver there; sdk/swift here is a submodule of it.
};

const [pkg, bump] = process.argv.slice(2);
if (!PACKAGES[pkg] || !bump) {
  console.error('usage: cut-release.mjs <browser|server|python> <patch|minor|major|x.y.z>');
  console.error('(swift releases from lohi-ai/agentray-swift: update VERSION and push main)');
  process.exit(2);
}

const sh = (cmd, args, opts = {}) => execFileSync(cmd, args, { encoding: 'utf8', ...opts }).trim();
const { dir, kind } = PACKAGES[pkg];

if (sh('git', ['status', '--porcelain'])) {
  console.error('working tree is dirty — commit or stash before cutting a release');
  process.exit(1);
}

// --- current version -------------------------------------------------------
const manifest = kind === 'npm' ? `${dir}/package.json` : `${dir}/pyproject.toml`;
const current = kind === 'npm'
  ? JSON.parse(readFileSync(manifest, 'utf8')).version
  : /^\s*version\s*=\s*["']([^"']+)["']/m.exec(readFileSync(manifest, 'utf8'))[1];

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

// The changelog is written with the change, not after it, so it must already
// name the version about to be tagged. Checked here rather than only in the
// release workflow: a tag whose tree fails its own gate is a tag you have to
// move, and moving tags is how a registry ends up with two builds of one
// version.
if (kind === 'npm') {
  const changelog = readFileSync(`${dir}/CHANGELOG.md`, 'utf8');
  if (!changelog.includes(next)) {
    console.error(`${dir}/CHANGELOG.md does not mention ${next} — write the entry (and the migration note) before tagging`);
    process.exit(1);
  }
}

// --- write, verify, commit -------------------------------------------
console.log(`${pkg}: ${current} -> ${next}`);

if (kind === 'npm') {
  const raw = readFileSync(manifest, 'utf8');
  writeFileSync(manifest, raw.replace(/("version":\s*)"[^"]+"/, `$1"${next}"`));
  // Keep the lockfile's own version field in step; npm rewrites it on install
  // anyway, and a lockfile that disagrees with its manifest is noise in a diff.
  execFileSync('npm', ['install', '--package-lock-only', '--no-audit', '--no-fund'], { cwd: dir, stdio: 'inherit' });
} else {
  const raw = readFileSync(manifest, 'utf8');
  writeFileSync(manifest, raw.replace(/^(\s*version\s*=\s*)["'][^"']+["']/m, `$1"${next}"`));
}

const changed = sh('git', ['diff', '--name-only']).split('\n').filter(Boolean);
if (changed.length) {
  execFileSync('git', ['add', ...changed], { stdio: 'inherit' });
  execFileSync('git', ['commit', '-m', `release: ${pkg} SDK ${next}`], { stdio: 'inherit' });
}

// Resolve the tag against the tree it will actually build, before creating it.
execFileSync('node', ['sdk/scripts/resolve-tag.mjs', tag], { stdio: 'inherit' });

console.log(`\nPrepared ${tag}. Nothing is published yet.\n\n  git push origin main\n\nMain CI verifies the exact commit once, creates the immutable tag after checks,\nthen publishes those same artifacts, documentation and download instructions.\nAn unchanged source version runs checks without creating another release.\n`);
