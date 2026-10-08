// Publish only verified artifacts from this run. A registry retry of an already
// public release uses its original immutable assets, never the rebuilt files.
import {execFileSync} from 'node:child_process';
import {readFileSync, writeFileSync, readdirSync, rmSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {resolve, basename} from 'node:path';
const plan = JSON.parse(process.env.RELEASE_PLAN);
const root = resolve('dist');
const gh = (...args) => execFileSync('gh',args,{encoding:'utf8',stdio:['ignore','pipe','pipe']}).trim();
if (plan.published) {
  for (const name of readdirSync(root)) rmSync(resolve(root,name));
  gh('release','download',plan.tag,'--dir',root);
  const sums = readdirSync(root).includes('checksums.txt') ? readFileSync(resolve(root,'checksums.txt'),'utf8') : ''; 
  for (const line of sums.trim().split('\n').filter(Boolean)) {
    const [,hash,file] = /^([0-9a-f]{64})  (.+)$/.exec(line) ?? [];
    if (!hash || file !== basename(file)) throw new Error('Invalid release checksum manifest');
    if (createHash('sha256').update(readFileSync(resolve(root,file))).digest('hex') !== hash) throw new Error(`Checksum mismatch: ${file}`);
  }
  console.log(`Registry retry uses original ${plan.tag} files`);
  process.exit(0);
}
const version = plan.version;
const archive = `agentray-${plan.pkg}-docs_${version}.tar.gz`;
execFileSync('tar',['-czf',resolve(root,archive),`${plan.dir}/README.md`,'docs/RELEASING-SDK.md',...(plan.pkg === 'python' ? [] : [`${plan.dir}/CHANGELOG.md`])]);
const assets = readdirSync(root).sort();
const packageFiles = assets.filter(file => file !== archive);
if (plan.kind === 'npm') {
  const expected = `${plan.name.replace('@','').replace('/','-')}-${version}.tgz`;
  if (!packageFiles.includes(expected)) throw new Error(`Missing verified package: ${expected}`);
  if (plan.pkg === 'browser' && !packageFiles.includes(`agentray-browser-${version}.min.js`)) throw new Error('Missing verified browser bundle');
} else if (!packageFiles.some(file=>file.endsWith('.whl')) || !packageFiles.some(file=>/^agentray-[0-9].*\.tar\.gz$/.test(file))) {
  throw new Error('Missing verified Python wheel/sdist');
}
const install = plan.kind === 'npm'
  ? `npm install https://github.com/${process.env.GITHUB_REPOSITORY}/releases/download/${plan.tag}/${assets.find(f=>f.endsWith('.tgz'))}`
  : `pip install https://github.com/${process.env.GITHUB_REPOSITORY}/releases/download/${plan.tag}/${assets.find(f=>f.endsWith('.whl'))}`;
writeFileSync(resolve(root,'DOWNLOADS.md'), `# ${plan.name} ${version}\n\nSource commit: \`${plan.commit}\`\n\n\`\`\`bash\n${install}\n\`\`\`\n\nDocumentation and migration notes are in \`${archive}\`.\n\n${assets.map(file=>`- [${file}](https://github.com/${process.env.GITHUB_REPOSITORY}/releases/download/${plan.tag}/${file})`).join('\n')}\n`);
const files = readdirSync(root).sort();
writeFileSync(resolve(root,'checksums.txt'),files.map(file=>`${createHash('sha256').update(readFileSync(resolve(root,file))).digest('hex')}  ${file}`).join('\n')+'\n');
let state;
try { state=JSON.parse(gh('release','view',plan.tag,'--json','isDraft')); }
catch(error) { if(!String(error.stderr).includes('release not found')) throw error; }
if(state && state.isDraft !== true) throw new Error('Refusing to replace published release assets');
if(!state) gh('release','create',plan.tag,'--verify-tag','--draft','--title',`${plan.name} ${version}`,...(plan.prerelease ? ['--prerelease'] : []));
gh('release','edit',plan.tag,'--notes-file',resolve(root,'DOWNLOADS.md'));
gh('release','upload',plan.tag,...readdirSync(root).map(file=>resolve(root,file)),'--clobber');
gh('release','edit',plan.tag,'--draft=false',...(plan.prerelease ? [] : ['--latest']));
