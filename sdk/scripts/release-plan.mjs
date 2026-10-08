import { execFileSync } from "node:child_process";
import { appendFileSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const stable = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*))?$/;
export function compareVersions(left, right) {
  const parse = value => {
    if (typeof value !== 'string' || !stable.test(value)) throw new Error('Expected semver');
    const [core, pre] = value.split(/-(.*)/s);
    return {core:core.split('.').map(BigInt),pre:pre?.split('.')};
  };
  const a=parse(left), b=parse(right);
  for(let i=0;i<3;i++) if(a.core[i] !== b.core[i]) return a.core[i] > b.core[i] ? 1 : -1;
  if (!a.pre || !b.pre) return a.pre ? -1 : b.pre ? 1 : 0;
  for(let i=0;i<Math.max(a.pre.length,b.pre.length);i++) {
    const x=a.pre[i], y=b.pre[i];
    if(x === y) continue;
    if(x === undefined || y === undefined) return x === undefined ? -1 : 1;
    const xn=/^\d+$/.test(x), yn=/^\d+$/.test(y);
    if(xn !== yn) return xn ? -1 : 1;
    return (xn ? BigInt(x) > BigInt(y) : x > y) ? 1 : -1;
  }
  return 0;
}

// Pure policy: an ordinary main push never invents a version. A retry can
// resume an unpublished tag on the same source commit without moving the tag.
export function releasePlan({version, pkg, refType, refName, commit, tags, published = false}) {
  compareVersions(version, version);
  if (!['browser','server','python'].includes(pkg)) throw new Error('Unknown SDK');
  const prefix = `${pkg}-v`;
  const tag = `${prefix}${version}`;
  if (refType === "tag" && refName !== tag) throw new Error("Tag must match source version");
  if (refType !== "tag" && (refType !== "branch" || refName !== "main")) throw new Error("Release requires main or a version tag");
  const existing = tags.find(t => t.tag === tag);
  const newer = tags.some(t => t.tag.startsWith(prefix) && stable.test(t.tag.slice(prefix.length)) && compareVersions(t.tag.slice(prefix.length), version) > 0);
  if (newer) throw new Error("Refusing a version older than an existing release tag");
  if (refType === "tag" && (!existing || existing.commit !== commit)) throw new Error("Tag does not resolve to the checked-out commit");
  if (refType === "branch" && existing && existing.commit !== commit)
    return {version, tag, commit, release: false, createTag: false};
  return {version, tag, commit, release: !published, createTag: !existing && !published};
}

export function sourceVersion(pkg) {
  if (!['browser','server','python'].includes(pkg)) throw new Error('Unknown SDK');
  if (pkg === 'python') return /^version\s*=\s*"([^"]+)"/m.exec(readFileSync('sdk/python/pyproject.toml','utf8'))?.[1];
  const json = path => JSON.parse(readFileSync(path,'utf8'));
  const manifest = json(`sdk/${pkg}/package.json`), lock = json(`sdk/${pkg}/package-lock.json`);
  if (manifest.version !== lock.version || manifest.version !== lock.packages?.[''].version) throw new Error('Package/lock versions must match');
  return manifest.version;
}

function main() {
  if (!process.env.GITHUB_OUTPUT) throw new Error('CI only');
  const git = (...args) => execFileSync('git',args,{encoding:'utf8'}).trim();
  const commit = git('rev-parse','HEAD');
  const refs = new Map(git('ls-remote','--tags','origin').split('\n').filter(Boolean).map(line => line.split(/\s+/).reverse()));
  const tags = [...refs].filter(([ref]) => /^refs\/tags\/(browser|server|python)-v/.test(ref) && !ref.endsWith('^{}'))
    .map(([ref,sha]) => ({tag:ref.slice('refs/tags/'.length),commit:refs.get(`${ref}^{}`) ?? sha}));
  const type = process.env.RELEASE_REF_TYPE, name = process.env.RELEASE_REF_NAME;
  const pkg = type === 'tag' ? /^(browser|server|python)-v/.exec(name)?.[1] : 'all';
  if (!pkg) throw new Error('Unknown SDK tag');
  const pending = [];
  for (const sdk of pkg === 'all' ? ['browser','server','python'] : [pkg]) {
    const version = sourceVersion(sdk);
    const input = {version,pkg:sdk,refType:type,refName:name,commit,tags};
    let plan = releasePlan(input), published = false;
    if (plan.release && !plan.createTag) {
      try {
        const release = JSON.parse(execFileSync('gh',['api',`repos/${process.env.GITHUB_REPOSITORY}/releases/tags/${plan.tag}`],{encoding:'utf8',stdio:['ignore','pipe','pipe']}));
        published = release.draft === false;
        plan = releasePlan({...input,published});
      } catch(error) {
        if (!String(error.stderr).includes('HTTP 404')) throw new Error('Cannot inspect release; fix GitHub access');
      }
    }
    // Explicit dispatch can finish optional registry publication using existing
    // immutable assets. Ordinary main/tag pushes never replace a published release.
    if (plan.release || (published && process.env.RETRY_PUBLISHED === 'true')) {
      const resolved = JSON.parse(execFileSync('node',['sdk/scripts/resolve-tag.mjs',plan.tag],{encoding:'utf8'}));
      pending.push({...plan,...resolved,published});
    }
    console.log(`${plan.tag}: ${plan.release ? 'pending' : 'unchanged / published'}`);
  }
  for(const [key,value] of Object.entries({commit,pkg,release:pending.length > 0,matrix:JSON.stringify(pending)}))
    appendFileSync(process.env.GITHUB_OUTPUT,`${key}=${value}\n`);
}
if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try { main(); } catch(error) { console.error(error.message); process.exitCode=1; }
}
