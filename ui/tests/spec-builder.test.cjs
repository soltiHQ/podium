const {test}=require('node:test');const assert=require('node:assert/strict');
const {builder,shortFingerprint}=require('../static/js/spec-builder.js');
const FP1='a'.repeat(64), FP2='b'.repeat(64), FP3='c'.repeat(64);
const catalog=[
  {api_version:'solti.io/v1',kind:'Wasm',fingerprints:[{fingerprint:FP1,agents:['a1','a2'],received:true},{fingerprint:FP2,agents:['a3'],received:true},{fingerprint:FP3,agents:['a4'],received:false}]},
  {api_version:'solti.io/v1',kind:'Container',fingerprints:[{fingerprint:FP3,agents:['a4'],received:true}]},
];
const sample={name:'worker',slot:'lane',version:4,workload:{api_version:'solti.io/v1',kind:'Wasm',schema_fingerprint:FP1,body:{module:'m.wasm',args:['x y','']}},targets:['a1'],target_labels:{env:'test'},runner_labels:{zone:'west'}};
function agent(id,kind,fp){return {id,capabilities:[{name:'r',workloads:[{group:'solti.io',version:'v1',kind,schema_fingerprint:fp}]}]};}
// A builder whose agents are known and whose form is filled, as after start() and selectType().
function ready(seed){const b=builder(seed,[],'/agents','/types');b.types=catalog;b.agents_info={a1:agent('a1','Wasm',FP1),a2:agent('a2','Wasm',FP1),a3:agent('a3','Wasm',FP2)};b.agents_opts=['a1','a2','a3'];b.type_key='solti.io/v1 Wasm';b.fingerprint=FP1;b.form={value:()=>(seed && seed.workload ? seed.workload.body : {}),problems:()=>[]};return b;}
function withFetch(fn,body){const old=global.fetch;global.fetch=fn;return Promise.resolve(body()).finally(()=>{global.fetch=old;});}

test('shortFingerprint is the first 12 characters',()=>{assert.equal(shortFingerprint(FP1),'aaaaaaaaaaaa');assert.equal(shortFingerprint('abc'),'abc');});

test('loadTypes accepts an items envelope and reports failures',async()=>{const b=builder(null,[],'/agents','/types');await withFetch(async()=>({ok:true,json:async()=>({items:catalog})}),()=>b.loadTypes());assert.equal(b.types.length,2);await withFetch(async()=>({ok:false,status:500}),()=>b.loadTypes());assert.match(b.types_error,/500/);});

test('agents first: a type is offered only when every selected agent declares it with one schema',()=>{const b=ready(null);b.agents=['a1','a2'];let c=b.candidates.find(x=>x.kind==='Wasm');assert.equal(c.fingerprint,FP1);assert.equal(c.reason,'');b.agents=['a1','a3'];c=b.candidates.find(x=>x.kind==='Wasm');assert.equal(c.fingerprint,'');assert.match(c.reason,/different schemas/);b.agents_info.a4={id:'a4',capabilities:[]};b.agents=['a1','a4'];assert.match(b.candidates.find(x=>x.kind==='Wasm').reason,/every selected agent/);});

test('labels mode takes the type and schema from the chosen group and sends only labels',()=>{const b=ready(null);b.target_mode='labels';b.agents=['a1'];b.label_rows=[{key:'env',value:'test'}];b.group_key='solti.io/v1 Wasm '+FP2;const c=b.candidates;assert.equal(c.length,1);assert.equal(c[0].fingerprint,FP2);b.fingerprint=FP2;const s=b.createSpec;assert.deepEqual(s.targets,[]);assert.deepEqual(s.target_labels,{env:'test'});assert.equal(s.workload.schema_fingerprint,FP2);});

test('the request carries only the workload and the envelope, never other fields',()=>{const b=ready(sample);const s=b.createSpec;assert.deepEqual(Object.keys(s).filter(k=>!['name','slot','jitter','restart_type','timeout_ms','interval_ms','backoff_first_ms','backoff_max_ms','backoff_factor','targets','target_labels','runner_labels','version','workload'].includes(k)),[]);assert.deepEqual(Object.keys(s.workload).sort(),['api_version','body','kind','schema_fingerprint']);assert.deepEqual(s.workload.body,sample.workload.body);});

test('a missing type or fingerprint blocks save',()=>{const b=builder(null,[],'/agents','/types');b.types=catalog;b.name='n';b.slot='s';assert.throws(()=>b.validate(),/workload type/i);});

test('a 400 shows the server problems as a list',async()=>{const b=ready(sample);await withFetch(async()=>({ok:false,status:400,headers:new Headers(),json:async()=>({message:'/args/0: got number, want string; /module: missing'})}),()=>b.submit('/api/v1/specs/x','PUT'));assert.deepEqual(b.problems,['/args/0: got number, want string','/module: missing']);assert.ok(b.error);await withFetch(async()=>({ok:true,status:204,headers:new Headers()}),()=>b.submit('/api/v1/specs/x','PUT'));assert.deepEqual(b.problems,[]);});

test('explicitly clearing targets, labels and intervals survives serialization',()=>{const b=ready(sample);b.agents=[];b.label_rows=[];b.runner_label_rows=[];b.interval_ms=5000;b.restart_type='never';const s=b.createSpec;assert.deepEqual(s.targets,[]);assert.deepEqual(s.target_labels,{});assert.deepEqual(s.runner_labels,{});assert.equal(s.interval_ms,0);});
test('duplicate labels rejected and prototype keys retained as own data',()=>{const b=ready(sample);b.target_mode='labels';b.label_rows=[{key:'x',value:'1'},{key:'x',value:'2'}];assert.throws(()=>b.createSpec,/Duplicate/);b.label_rows=[{key:'__proto__',value:'safe'}];assert.equal(JSON.parse(JSON.stringify(b.createSpec)).target_labels.__proto__,'safe');});
test('conflict and network errors preserve drafts and allow retry',async()=>{const b=ready(sample);b.name='unsaved';await withFetch(async()=>({ok:false,status:409,headers:new Headers(),json:async()=>({message:'Version conflict'})}),()=>b.submit('/spec','PUT'));assert.equal(b.name,'unsaved');assert.ok(b.conflict);assert.equal(b.submitting,false);await withFetch(async()=>{throw Error('offline')},()=>b.submit('/spec','PUT'));assert.equal(b.name,'unsaved');assert.match(b.error,/retained/);assert.equal(b.submitting,false);});
test('agent picker follows cursor pages and deduplicates IDs',async()=>{const b=builder(null,[],'/agents','/types');let n=0;await withFetch(async()=>({ok:true,json:async()=>++n===1?{items:[{id:'a'}],next_cursor:'next'}:{items:[{id:'a'},{id:'b'}]}}),()=>b.loadAgents());assert.deepEqual(b.agents_opts,['a','b']);});
test('spec save sends its JSON body as application/json',async()=>{const b=ready(sample);let sent;await withFetch(async(url,init)=>{sent=init;return {ok:true,status:204,headers:new Headers()};},()=>b.submit('/api/v1/specs/x','PUT'));assert.equal(sent.method,'PUT');assert.equal(sent.headers['Content-Type'],'application/json');assert.equal(JSON.parse(sent.body).name,'worker');assert.equal(JSON.parse(sent.body).workload.kind,'Wasm');});
