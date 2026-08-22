// Turns a release tag into the facts every release job needs, and refuses to
// go further if the tag disagrees with the manifest it claims to release.
//
//   node sdk/scripts/resolve-tag.mjs browser-v0.2.0
//   -> {"pkg":"browser","dir":"sdk/browser","version":"0.2.0","prerelease":false,...}
//
// The SDKs version independently -- a fix to the server client is not a reason
// to bump the browser one -- so a bare `v0.2.0` is ambiguous and is rejected. The manifest check exists because `npm version` writes package.json
// and the tag together, but a hand-written tag, an amended commit, or a
// cherry-pick can separate them; publishing 0.2.0 from a tree that says 0.1.0
// is the kind of mistake a registry will not let you take back.
import { readFileSync } from 'node:fs';

const PACKAGES = {
  browser: { dir: 'sdk/browser', kind: 'npm', name: '@agentray/browser' },
  server: { dir: 'sdk/server', kind: 'npm', name: '@agentray/server' },
  python: { dir: 'sdk/python', kind: 'pypi', name: 'agentray' },
  // No swift: the Swift SDK is its own repository (lohi-ai/agentray-swift,
  // carried here as a submodule at sdk/swift) and releases itself from there.
  // SwiftPM resolves versions from that repo's tags, so a swift tag here would
  // publish nothing.
};

const tag = process.argv[2];
if (!tag) {
  console.error('usage: resolve-tag.mjs <tag>   e.g. browser-v0.2.0');
  process.exit(2);
}

const m = /^(browser|server|python)-v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$/.exec(tag);
if (!m) {
  console.error(`unrecognised release tag: ${tag}`);
  console.error('expected <browser|server|python>-v<semver>, e.g. browser-v0.2.0');
  console.error('(a bare v0.2.0 is a product tag, not an SDK release -- the SDKs version independently)');
  console.error('(the Swift SDK releases from lohi-ai/agentray-swift, tagged there as bare semver)');
  process.exit(1);
}

const [, pkg, version] = m;
const { dir, kind, name } = PACKAGES[pkg];
const root = new URL('../../', import.meta.url).pathname;

let manifestVersion = null;
if (kind === 'npm') {
  manifestVersion = JSON.parse(readFileSync(`${root}${dir}/package.json`, 'utf8')).version;
} else if (kind === 'pypi') {
  const toml = readFileSync(`${root}${dir}/pyproject.toml`, 'utf8');
  manifestVersion = /^\s*version\s*=\s*["']([^"']+)["']/m.exec(toml)?.[1] ?? null;
}

if (manifestVersion !== null && manifestVersion !== version) {
  console.error(`tag ${tag} says ${version}, but ${dir} says ${manifestVersion}`);
  console.error('bump the manifest and re-tag -- do not publish a version the tree does not claim');
  process.exit(1);
}

const prerelease = version.includes('-');
const out = {
  pkg,
  dir,
  kind,
  name,
  version,
  tag,
  prerelease,
  // npm dist-tag: a prerelease must never become what `npm i <pkg>` installs.
  npmTag: prerelease ? 'next' : 'latest',
};

console.log(JSON.stringify(out));
