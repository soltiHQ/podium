const {test}=require('node:test');const assert=require('node:assert/strict');
const {builder,labels,shortFingerprint}=require('../static/js/spec-builder.js');
const FP1='a'.repeat(64), FP2='b'.repeat(64), FP3='c'.repeat(64);
const catalog=[
  {api_version:'solti.io/v1',kind:'Wasm',fingerprints:[{fingerprint:FP1,agents:['a1','a2'],received:true},{fingerprint:FP2,agents:['a3'],received:true},{fingerprint:FP3,agents:['a4'],received:false}]},
  {api_version:'solti.io/v1',kind:'Container',fingerprints:[{fingerprint:FP3,agents:['a4'],received:true}]},
];
const sample={name:'worker',slot:'lane',version:4,workload:{api_version:'solti.io/v1',kind:'Wasm',schema_fingerprint:FP1,body:{module:'m.wasm',args:['x y','']}},targets:['a1'],target_labels:{env:'test'},runner_labels:{zone:'west'}};
function withTypes(seed){const b=builder(seed,[],'/agents','/types');b.types=catalog;return b;}
function withFetch(fn,body){const old=global.fetch;global.fetch=fn;return Promise.resolve(body()).finally(()=>{global.fetch=old;});}

test('shortFingerprint is the first 12 characters',()=>{assert.equal(shortFingerprint(FP1),'aaaaaaaaaaaa');assert.equal(shortFingerprint('abc'),'abc');});

test('create mode starts with an empty object body and no kind fields',()=>{const b=builder(null,[],'/agents','/types');assert.equal(b.body_json,'{}');assert.equal(b.type_key,'');for(const k of ['kind_type','kind_config','script_body','wasm_json','container_json','subprocess_mode','env_rows'])assert.equal(k in b,false,k);});

test('type loading selects nothing and lists received fingerprints with agent counts',async()=>{const b=builder(null,[],'/agents','/types');await withFetch(async()=>({ok:true,json:async()=>catalog}),()=>b.loadTypes());assert.equal(b.types.length,2);b.type_key=b.typeOptions.find(o=>o.label==='solti.io/v1 Wasm').key;const fps=b.fingerprintOptions;assert.equal(fps.length,2);assert.match(fps[0].label,/^aaaaaaaaaaaa · 2 agents$/);assert.match(fps[1].label,/^bbbbbbbbbbbb · 1 agent$/);assert.equal(fps.some(f=>f.value===FP3),false);});

test('loadTypes accepts an items envelope and reports failures',async()=>{const b=builder(null,[],'/agents','/types');await withFetch(async()=>({ok:true,json:async()=>({items:catalog})}),()=>b.loadTypes());assert.equal(b.types.length,2);await withFetch(async()=>({ok:false,status:500}),()=>b.loadTypes());assert.match(b.types_error,/500/);});

test('a type change picks the first fingerprint, resets the body and keeps other fields',()=>{const b=withTypes(null);b.name='n';b.body_json='{"x":1}';b.type_key=b.typeOptions.find(o=>o.label==='solti.io/v1 Wasm').key;b.onTypeChange();assert.equal(b.fingerprint,FP1);assert.equal(b.body_json,'{}');b.type_key=b.typeOptions.find(o=>o.label==='solti.io/v1 Container').key;b.onTypeChange();assert.equal(b.fingerprint,FP3);assert.equal(b.name,'n');});

test('edit preselects the type and fingerprint and pretty-prints the body',()=>{const b=withTypes(sample);assert.equal(b.fingerprint,FP1);assert.equal(b.typeOptions.find(o=>o.key===b.type_key).label,'solti.io/v1 Wasm');assert.deepEqual(JSON.parse(b.body_json),sample.workload.body);assert.match(b.body_json,/\n  "module"/);assert.deepEqual(b.createSpec.workload,sample.workload);assert.equal(b.createSpec.version,4);});

test('an edited fingerprint missing from the catalog stays selectable, marked as having no agents',()=>{const seed={...sample,workload:{...sample.workload,schema_fingerprint:'d'.repeat(64)}};const b=withTypes(seed);const o=b.fingerprintOptions.find(f=>f.value==='d'.repeat(64));assert.ok(o);assert.match(o.label,/no agents/);assert.equal(b.createSpec.workload.schema_fingerprint,'d'.repeat(64));
  const gone={...sample,workload:{api_version:'x.io/v2',kind:'Gone',schema_fingerprint:FP2,body:{}}};const g=withTypes(gone);assert.equal(g.typeOptions.find(o=>o.key===g.type_key).label,'x.io/v2 Gone');assert.match(g.fingerprintOptions[0].label,/no agents/);});

test('the request carries only the workload and the envelope, never kind fields',()=>{const b=withTypes(sample);const s=b.createSpec;assert.equal('kind_type' in s,false);assert.equal('kind_config' in s,false);assert.deepEqual(Object.keys(s.workload).sort(),['api_version','body','kind','schema_fingerprint']);});

test('invalid JSON, arrays and scalars block save',()=>{const b=withTypes(sample);for(const bad of ['{oops','[]','"s"','3','null','']){b.body_json=bad;assert.throws(()=>b.validate(),/JSON/,bad);}b.body_json='{"a":1}';assert.deepEqual(b.validate().workload.body,{a:1});});

test('invalid JSON shows an error and sends nothing',async()=>{const b=withTypes(sample);b.body_json='{oops';let called=false;await withFetch(async()=>{called=true;return {ok:true,status:204,headers:new Headers()};},()=>b.submit('/api/v1/specs/x','PUT'));assert.equal(called,false);assert.match(b.error,/JSON/);assert.equal(b.submitting,false);});

test('a missing type or fingerprint blocks save',()=>{const b=withTypes(null);b.name='n';b.slot='s';assert.throws(()=>b.validate(),/workload type/i);});

test('a 400 shows the server problems as a list',async()=>{const b=withTypes(sample);await withFetch(async()=>({ok:false,status:400,headers:new Headers(),json:async()=>({message:'/args/0: got number, want string; /module: missing'})}),()=>b.submit('/api/v1/specs/x','PUT'));assert.deepEqual(b.problems,['/args/0: got number, want string','/module: missing']);assert.ok(b.error);assert.equal(b.body_json.includes('m.wasm'),true);await withFetch(async()=>({ok:true,status:204,headers:new Headers()}),()=>b.submit('/api/v1/specs/x','PUT'));assert.deepEqual(b.problems,[]);});

test('agent options are the union of the type\'s fingerprints, limited to known agents',()=>{const b=withTypes(null);b.type_key=b.typeOptions.find(o=>o.label==='solti.io/v1 Wasm').key;assert.deepEqual(b.agentOptions,['a1','a2','a3','a4']);b.agents_opts=['a2','a3','zz'];assert.deepEqual(b.agentOptions,['a2','a3']);b.agent_error='down';assert.deepEqual(b.agentOptions,['a1','a2','a3','a4']);b.type_key='';assert.deepEqual(b.agentOptions,[]);});

test('explicitly clearing targets, labels and intervals survives serialization',()=>{const b=withTypes(sample);b.agents=[];b.label_rows=[];b.runner_label_rows=[];b.interval_ms=5000;b.restart_type='never';const s=b.createSpec;assert.deepEqual(s.targets,[]);assert.deepEqual(s.target_labels,{});assert.deepEqual(s.runner_labels,{});assert.equal(s.interval_ms,0);});
test('duplicate labels rejected and prototype keys retained as own data',()=>{const b=withTypes(sample);b.label_rows=[{key:'x',value:'1'},{key:'x',value:'2'}];assert.throws(()=>b.createSpec,/Duplicate/);b.label_rows=[{key:'__proto__',value:'safe'}];assert.equal(JSON.parse(JSON.stringify(b.createSpec)).target_labels.__proto__,'safe');});
test('conflict and network errors preserve drafts and allow retry',async()=>{const b=withTypes(sample);b.name='unsaved';await withFetch(async()=>({ok:false,status:409,headers:new Headers(),json:async()=>({message:'Version conflict'})}),()=>b.submit('/spec','PUT'));assert.equal(b.name,'unsaved');assert.ok(b.conflict);assert.equal(b.submitting,false);await withFetch(async()=>{throw Error('offline')},()=>b.submit('/spec','PUT'));assert.equal(b.name,'unsaved');assert.match(b.error,/retained/);assert.equal(b.submitting,false);});
test('agent picker follows cursor pages and deduplicates IDs',async()=>{const b=builder(null,[],'/agents','/types');let n=0;await withFetch(async()=>({ok:true,json:async()=>++n===1?{items:[{id:'a'}],next_cursor:'next'}:{items:[{id:'a'},{id:'b'}]}}),()=>b.loadAgents());assert.deepEqual(b.agents_opts,['a','b']);});
test('spec save sends its JSON body as application/json',async()=>{const b=withTypes(sample);let sent;await withFetch(async(url,init)=>{sent=init;return {ok:true,status:204,headers:new Headers()};},()=>b.submit('/api/v1/specs/x','PUT'));assert.equal(sent.method,'PUT');assert.equal(sent.headers['Content-Type'],'application/json');assert.equal(JSON.parse(sent.body).name,'worker');assert.equal(JSON.parse(sent.body).workload.kind,'Wasm');});
