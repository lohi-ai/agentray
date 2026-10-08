import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,readdirSync,rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join,resolve,basename} from 'node:path';
import {spawnSync} from 'node:child_process';
const script=resolve('sdk/scripts/publish-release.mjs');
function fixture(t,mode) {
  const root=mkdtempSync(join(tmpdir(),'sdk-release-test-'));
  t.after(()=>rmSync(root,{recursive:true,force:true}));
  for(const dir of ['dist','bin','sdk/browser','docs']) mkdirSync(join(root,dir),{recursive:true});
  for(const file of ['sdk/browser/README.md','sdk/browser/CHANGELOG.md','docs/RELEASING-SDK.md']) writeFileSync(join(root,file),'documentation');
  writeFileSync(join(root,'dist/agentray-browser-1.2.0.tgz'),'checked-tarball');
  writeFileSync(join(root,'dist/agentray-browser-1.2.0.min.js'),'checked-bundle');
  writeFileSync(join(root,'bin/gh'),String.raw`#!/usr/bin/env node
const fs=require('fs'); const args=process.argv.slice(2);
fs.appendFileSync(process.env.CALL_LOG,JSON.stringify(args)+'\n');
if(args[1]==='view') {
  if(process.env.TEST_MODE==='published') console.log('{"isDraft":false}');
  else if(process.env.TEST_MODE==='network') {console.error('HTTP 503');process.exit(1);}
  else {console.error('release not found');process.exit(1);}
}
if(args[1]==='download') {
  const dir=args[args.indexOf('--dir')+1];
  fs.writeFileSync(dir+'/agentray-browser-1.2.0.tgz','original-tarball');
}
`,{mode:0o755});
  const plan={kind:'npm',pkg:'browser',name:'@agentray/browser',dir:'sdk/browser',version:'1.2.0',tag:'browser-v1.2.0',commit:'abc',prerelease:false,published:false};
  const run=(options={})=>spawnSync(process.execPath,[script],{cwd:root,encoding:'utf8',env:{...process.env,PATH:join(root,'bin')+':'+process.env.PATH,TEST_MODE:mode,CALL_LOG:join(root,'calls.jsonl'),GITHUB_REPOSITORY:'lohi-ai/agentray',RELEASE_PLAN:JSON.stringify({...plan,...options})}});
  const calls=()=>readFileSync(join(root,'calls.jsonl'),'utf8').trim().split('\n').map(line=>JSON.parse(line));
  return {root,run,calls};
}
test('all verified assets and checksums are attached before publishing a draft', t=>{
  const f=fixture(t,'new'); const result=f.run(); assert.equal(result.status,0,result.stderr);
  const calls=f.calls();
  assert.ok(calls.find(args=>args[1]==='create').includes('--draft'));
  const upload=calls.find(args=>args[1]==='upload');
  for(const name of readdirSync(join(f.root,'dist'))) assert.ok(upload.map(value=>basename(value)).includes(name));
  assert.ok(calls.at(-1).includes('--draft=false'));
  assert.match(readFileSync(join(f.root,'dist/DOWNLOADS.md'),'utf8'),/abc/);
});
test('a concurrently published release is never overwritten', t=>{
  const f=fixture(t,'published');const result=f.run();assert.notEqual(result.status,0);assert.match(result.stderr,/Refusing to replace/);
  assert.ok(!f.calls().some(args=>['create','upload','edit'].includes(args[1])));
});
test('registry retry downloads original public artifacts without editing release', t=>{
  const f=fixture(t,'published');const result=f.run({published:true});assert.equal(result.status,0,result.stderr);
  assert.equal(readFileSync(join(f.root,'dist/agentray-browser-1.2.0.tgz'),'utf8'),'original-tarball');
  assert.deepEqual(f.calls().map(args=>args[1]),['download']);
});
test('missing required bundle and API failures cannot publish', t=>{
  const f=fixture(t,'network');rmSync(join(f.root,'dist/agentray-browser-1.2.0.min.js'));
  assert.match(f.run().stderr,/Missing verified browser bundle/);
  writeFileSync(join(f.root,'dist/agentray-browser-1.2.0.min.js'),'checked-bundle');
  const result=f.run();assert.notEqual(result.status,0);assert.match(result.stderr,/HTTP 503/);
  assert.ok(!f.calls().some(args=>['create','upload','edit'].includes(args[1])));
});
