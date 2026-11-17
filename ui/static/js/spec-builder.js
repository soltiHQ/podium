(function (root) {
  'use strict';
  function shortFingerprint(fp) { return String(fp || '').slice(0, 12); }
  function labels(rows) { const out={}; for (const r of rows) { const k=r.key.trim(); if (!k) { if(r.value) throw new Error('A label needs a key.'); continue; } if(Object.hasOwn(out,k)) throw new Error('Duplicate key: '+k); Object.defineProperty(out,k,{value:r.value,enumerable:true,writable:true,configurable:true}); } return out; }
  function typeKey(apiVersion, kind) { return apiVersion+' '+kind; }
  function plural(n, word) { return n+' '+word+(n===1?'':'s'); }
  function builder(seed, presets, endpoint, typesEndpoint) {
    const base={name:'',slot:'',type_key:'',fingerprint:'',body_json:'{}',types:[],types_error:'',problems:[],timeout_ms:30000,restart_type:'never',interval_ms:0,backoff_preset:'standard',jitter:'none',backoff_first_ms:1000,backoff_max_ms:5000,backoff_factor:2,loaded_version:0,agents:[],agents_opts:[],agents_open:false,target_mode:'agents',label_rows:[],runner_label_rows:[],error:'',agent_error:'',conflict:false,submitting:false,presets,agents_endpoint:endpoint,types_endpoint:typesEndpoint,seed_workload:null};
    if(seed) {
      for(const k of ['name','slot','timeout_ms','restart_type','interval_ms','jitter','backoff_first_ms','backoff_max_ms','backoff_factor']) if(seed[k]!=null) base[k]=seed[k];
      base.loaded_version=seed.version || 0;
      const w=seed.workload;
      if(w) { base.seed_workload={api_version:w.api_version,kind:w.kind,schema_fingerprint:w.schema_fingerprint}; base.type_key=typeKey(w.api_version,w.kind); base.fingerprint=w.schema_fingerprint || ''; base.body_json=JSON.stringify(w.body || {},null,2); }
      base.agents=(seed.targets || []).slice(); base.label_rows=Object.entries(seed.target_labels || {}).map(([key,value])=>({key,value})); base.runner_label_rows=Object.entries(seed.runner_labels || {}).map(([key,value])=>({key,value})); base.backoff_preset='custom';
    }
    return {...base,
      // The catalog's types, plus the edited spec's own type when the catalog no longer lists it.
      get typeList() {
        const list=this.types.map(t=>({api_version:t.api_version,kind:t.kind,fingerprints:t.fingerprints || []}));
        const w=this.seed_workload;
        if(w && !list.some(t=>t.api_version===w.api_version && t.kind===w.kind)) list.push({api_version:w.api_version,kind:w.kind,fingerprints:[]});
        return list;
      },
      get typeOptions() { return this.typeList.map(t=>({key:typeKey(t.api_version,t.kind),label:t.api_version+' '+t.kind})); },
      get currentType() { return this.typeList.find(t=>typeKey(t.api_version,t.kind)===this.type_key) || null; },
      // Only fingerprints whose schema has arrived are selectable. The edited
      // spec's own fingerprint stays selectable: the server still holds its schema.
      get fingerprintOptions() {
        const t=this.currentType; if(!t) return [];
        const out=t.fingerprints.filter(f=>f.received).map(f=>({value:f.fingerprint,label:shortFingerprint(f.fingerprint)+' · '+plural((f.agents || []).length,'agent'),title:f.fingerprint}));
        const w=this.seed_workload;
        if(w && w.schema_fingerprint && typeKey(w.api_version,w.kind)===this.type_key && !out.some(o=>o.value===w.schema_fingerprint)) out.push({value:w.schema_fingerprint,label:shortFingerprint(w.schema_fingerprint)+' · no agents',title:w.schema_fingerprint});
        return out;
      },
      // Agents declaring the type under any of its fingerprints, limited to the
      // known agents once that list has loaded.
      get agentOptions() {
        const t=this.currentType; if(!t) return [];
        const union=[...new Set(t.fingerprints.flatMap(f=>f.agents || []))].sort();
        if(this.agent_error || this.agents_opts.length===0) return union;
        return union.filter(a=>this.agents_opts.includes(a));
      },
      onTypeChange() { const o=this.fingerprintOptions; this.fingerprint=o.length?o[0].value:''; this.body_json='{}'; this.problems=[]; },
      get workloadBody() {
        let v;
        try { v=JSON.parse(this.body_json); } catch(e) { throw new Error('The workload body is not valid JSON: '+e.message); }
        if(!v || Array.isArray(v) || typeof v!=='object') throw new Error('The workload body must be a JSON object (valid JSON, but not an object or array).');
        return v;
      },
      get workload() {
        const t=this.currentType;
        return {api_version:t?t.api_version:'',kind:t?t.kind:'',schema_fingerprint:this.fingerprint,body:this.workloadBody};
      },
      get targetLabels(){ return labels(this.label_rows); }, get runnerLabels(){return labels(this.runner_label_rows);},
      get createSpec(){return {name:this.name.trim(),slot:this.slot.trim(),workload:this.workload,timeout_ms:Number(this.timeout_ms),restart_type:this.restart_type,interval_ms:this.restart_type==='always'?Number(this.interval_ms):0,jitter:this.jitter,backoff_first_ms:Number(this.backoff_first_ms),backoff_max_ms:Number(this.backoff_max_ms),backoff_factor:Number(this.backoff_factor),version:this.loaded_version,targets:this.agents.slice(),target_labels:this.targetLabels,runner_labels:this.runnerLabels};},
      get previewJSON(){try{return JSON.stringify(this.createSpec,null,2);}catch(e){return e.message;}},
      validate(){if(!this.currentType || !this.fingerprint) throw new Error('Pick a workload type and a schema fingerprint.');const s=this.createSpec;if(!s.name || !s.slot) throw new Error('Name and slot are required.'); for(const [k,v] of Object.entries(s)) if(typeof v==='number' && !Number.isFinite(v)) throw new Error(k+' must be a finite number.'); if(!Number.isSafeInteger(s.timeout_ms) || s.timeout_ms<=0) throw new Error('Timeout must be a positive integer.'); if(s.backoff_first_ms<=0 || !Number.isSafeInteger(s.backoff_first_ms) || s.backoff_max_ms<s.backoff_first_ms || !Number.isSafeInteger(s.backoff_max_ms) || s.backoff_factor<1) throw new Error('Backoff needs positive integer durations, maximum ≥ first, and factor ≥ 1.'); if(s.restart_type==='always' && (s.interval_ms<=0 || !Number.isSafeInteger(s.interval_ms))) throw new Error('Always restart needs a positive integer interval.'); return s;},
      async loadTypes(){this.types_error='';try{const r=await fetch(this.types_endpoint,{headers:{Accept:'application/json'}});if(!r.ok)throw new Error('Workload types unavailable ('+r.status+').');const d=await r.json();this.types=Array.isArray(d)?d:(d.items || []);}catch(e){this.types_error=e.message;}},
      async loadAgents(){this.agent_error='';try{let url=this.agents_endpoint;const seen=new Set(),items=[];for(let page=0;url && page<100;page++){const r=await fetch(url,{headers:{Accept:'application/json'}});if(!r.ok)throw new Error('Agents unavailable ('+r.status+'). You can still edit existing targets.');const d=await r.json();for(const a of d.items || [])if(!seen.has(a.id)){seen.add(a.id);items.push(a.id);}url=d.next_cursor?this.agents_endpoint+'?cursor='+encodeURIComponent(d.next_cursor):'';}this.agents_opts=items;}catch(e){this.agent_error=e.message;}},
      async submit(endpoint,method){if(this.submitting)return;this.error='';this.problems=[];this.conflict=false;let body;try{body=this.validate();}catch(e){this.error=e.message;return;}this.submitting=true;try{const r=await fetch(endpoint,{method,headers:{'Content-Type':'application/json',Accept:'application/json','HX-Request':'true'},body:JSON.stringify(body)});const redirect=r.headers.get('HX-Redirect');if(r.status===401){root.location.href=redirect || '/login';return;}if(r.ok){if(redirect)root.location.href=redirect;return;}let d={};try{d=await r.json();}catch{}this.conflict=r.status===409;if(r.status===400 && d.message){this.error='The server rejected the spec. Your edits are retained.';this.problems=String(d.message).split('; ').filter(Boolean);}else{this.error=d.message || 'Save failed ('+r.status+'). Your edits are retained.';if(this.conflict)this.error+=' Your edits are retained. Open the latest version in another tab before reconciling changes.';}}catch{this.error='Connection failed. Your edits are retained; retry when the server is available.';}finally{this.submitting=false;}},
      applyPreset(name){const p=this.presets.find(p=>p.label.toLowerCase()===name);if(p){this.jitter=p.jitter;this.backoff_first_ms=p.firstMs;this.backoff_max_ms=p.maxMs;this.backoff_factor=p.factor;}this.backoff_preset=name;}
    };
  }
  root.soltiSpecBuilder=builder;
  if(typeof module!=='undefined')module.exports={builder,labels,shortFingerprint};
})(typeof window==='undefined'?globalThis:window);
