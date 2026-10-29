(function (root) {
  'use strict';
  function encode(s) { const bytes = new TextEncoder().encode(s); let raw=''; for (const b of bytes) raw+=String.fromCharCode(b); return btoa(raw); }
  function decode(s) { return new TextDecoder('utf-8', {fatal:true}).decode(Uint8Array.from(atob(s || ''), c=>c.charCodeAt(0))); }
  function objectJSON(text, label) { const obj=JSON.parse(text); if (!obj || Array.isArray(obj) || typeof obj!=='object') throw new Error(label+' must be a JSON object.'); return obj; }
  function argv(text) { const value=JSON.parse(text || '[]'); if (!Array.isArray(value) || value.some(v=>typeof v!=='string')) throw new Error('Arguments must be a JSON array of strings.'); return value; }
  function labels(rows) { const out={}; for (const r of rows) { const k=r.key.trim(); if (!k) { if(r.value) throw new Error('A label or environment variable needs a key.'); continue; } if(Object.hasOwn(out,k)) throw new Error('Duplicate key: '+k); Object.defineProperty(out,k,{value:r.value,enumerable:true,writable:true,configurable:true}); } return out; }
  function builder(seed, presets, endpoint) {
    const base={name:'',slot:'',kind_type:'subprocess',timeout_ms:30000,restart_type:'never',interval_ms:0,backoff_preset:'standard',jitter:'none',backoff_first_ms:1000,backoff_max_ms:5000,backoff_factor:2,loaded_version:0,subprocess_mode:'command',args:'[]',env_rows:[],cwd:'',fail_on_non_zero:true,cmd:'',script_interpreter:'bash',script_body:'',script_drag_over:false,wasm_json:'{}',container_json:'{}',agents:[],agents_opts:[],agents_open:false,target_mode:'agents',label_rows:[],runner_label_rows:[],error:'',agent_error:'',conflict:false,submitting:false,legacy_script_blocked:false,legacy_script_json:'',presets,agents_endpoint:endpoint};
    if(seed) {
      for(const k of ['name','slot','kind_type','timeout_ms','restart_type','interval_ms','jitter','backoff_first_ms','backoff_max_ms','backoff_factor']) if(seed[k]!=null) base[k]=seed[k];
      base.loaded_version=seed.version || 0;
      const kc=seed.kind_config || {};
      if(base.kind_type==='subprocess') {
        const mode=kc.mode || {command:kc.command,script:kc.script};
        if(mode.script) { base.subprocess_mode='script'; base.script_interpreter=mode.script.interpreter || ({SCRIPT_RUNTIME_BASH:'bash',SCRIPT_RUNTIME_PYTHON:'python3',SCRIPT_RUNTIME_NODE:'node'}[mode.script.wellKnown]) || mode.script.custom?.command || ''; if(!mode.script.interpreter && (mode.script.wellKnown || mode.script.custom)) {base.legacy_script_blocked=true;base.legacy_script_json=JSON.stringify(mode.script,null,2);base.error='Legacy scripts used an inline runtime. Review the interpreter and arguments before switching to file execution.';} base.script_body=decode(mode.script.body); base.args=JSON.stringify(mode.script.args || []); }
        else { base.cmd=(mode.command || {}).command || ''; base.args=JSON.stringify((mode.command || {}).args || []); }
        base.env_rows=(kc.env || []).map(e=>({key:e.key,value:e.value})); base.cwd=kc.cwd || ''; base.fail_on_non_zero=kc.failOnNonZero ?? false;
      } else if(base.kind_type==='wasm') base.wasm_json=JSON.stringify(kc,null,2);
      else if(base.kind_type==='container') base.container_json=JSON.stringify(kc,null,2);
      base.agents=(seed.targets || []).slice(); base.label_rows=Object.entries(seed.target_labels || {}).map(([key,value])=>({key,value})); base.runner_label_rows=Object.entries(seed.runner_labels || {}).map(([key,value])=>({key,value})); base.backoff_preset='custom';
    }
    return {...base,
      get kindConfig() {
        if(this.kind_type==='wasm') return objectJSON(this.wasm_json,'WASM configuration');
        if(this.kind_type==='container') return objectJSON(this.container_json,'Container configuration');
        const args=argv(this.args), cfg={mode:{},failOnNonZero:this.fail_on_non_zero};
        if(this.subprocess_mode==='script') cfg.mode.script={interpreter:this.script_interpreter,body:encode(this.script_body),args};
        else cfg.mode.command={command:this.cmd,args};
        cfg.env=this.env_rows.filter(r=>r.key!=='' || r.value!=='').map(r=>{if(!r.key)throw new Error('Environment variables need a key.');return {key:r.key,value:r.value};}); if(this.cwd) cfg.cwd=this.cwd; return cfg;
      },
      get targetLabels(){ return labels(this.label_rows); }, get runnerLabels(){return labels(this.runner_label_rows);},
      get createSpec(){return {name:this.name.trim(),slot:this.slot.trim(),kind_type:this.kind_type,kind_config:this.kindConfig,timeout_ms:Number(this.timeout_ms),restart_type:this.restart_type,interval_ms:this.restart_type==='always'?Number(this.interval_ms):0,jitter:this.jitter,backoff_first_ms:Number(this.backoff_first_ms),backoff_max_ms:Number(this.backoff_max_ms),backoff_factor:Number(this.backoff_factor),version:this.loaded_version,targets:this.agents.slice(),target_labels:this.targetLabels,runner_labels:this.runnerLabels};},
      get previewJSON(){try{return JSON.stringify(this.createSpec,null,2);}catch(e){return e.message;}},
      validate(){if(this.legacy_script_blocked)throw new Error('Review the legacy script and explicitly switch to the current interpreter contract before saving.');const s=this.createSpec;if(!s.name || !s.slot) throw new Error('Name and slot are required.'); for(const [k,v] of Object.entries(s)) if(typeof v==='number' && !Number.isFinite(v)) throw new Error(k+' must be a finite number.'); if(!Number.isSafeInteger(s.timeout_ms) || s.timeout_ms<=0) throw new Error('Timeout must be a positive integer.'); if(s.backoff_first_ms<=0 || !Number.isSafeInteger(s.backoff_first_ms) || s.backoff_max_ms<s.backoff_first_ms || !Number.isSafeInteger(s.backoff_max_ms) || s.backoff_factor<1) throw new Error('Backoff needs positive integer durations, maximum ≥ first, and factor ≥ 1.'); if(s.restart_type==='always' && (s.interval_ms<=0 || !Number.isSafeInteger(s.interval_ms))) throw new Error('Always restart needs a positive integer interval.'); if(s.kind_type==='subprocess') { if(this.subprocess_mode==='command' && !this.cmd.trim()) throw new Error('Command is required.'); if(this.subprocess_mode==='script' && (!this.script_interpreter.trim() || !this.script_body)) throw new Error('Interpreter and script body are required.'); } return s;},
      async loadAgents(){this.agent_error='';try{let url=this.agents_endpoint;const seen=new Set(),items=[];for(let page=0;url && page<100;page++){const r=await fetch(url,{headers:{Accept:'application/json'}});if(!r.ok)throw new Error('Agents unavailable ('+r.status+'). You can still edit existing targets.');const d=await r.json();for(const a of d.items || [])if(!seen.has(a.id)){seen.add(a.id);items.push(a.id);}url=d.next_cursor?this.agents_endpoint+'?cursor='+encodeURIComponent(d.next_cursor):'';}this.agents_opts=items;}catch(e){this.agent_error=e.message;}},
      async submit(endpoint,method){if(this.submitting)return;this.error='';this.conflict=false;let body;try{body=this.validate();}catch(e){this.error=e.message;return;}this.submitting=true;try{const r=await fetch(endpoint,{method,headers:{'Content-Type':'application/json',Accept:'application/json','HX-Request':'true'},body:JSON.stringify(body)});const redirect=r.headers.get('HX-Redirect');if(r.status===401){root.location.href=redirect || '/login';return;}if(r.ok){if(redirect)root.location.href=redirect;return;}let d={};try{d=await r.json();}catch{}this.conflict=r.status===409;this.error=d.message || 'Save failed ('+r.status+'). Your edits are retained.';if(this.conflict)this.error+=' Your edits are retained. Open the latest version in another tab before reconciling changes.';}catch{this.error='Connection failed. Your edits are retained; retry when the server is available.';}finally{this.submitting=false;}},
      applyPreset(name){const p=this.presets.find(p=>p.label.toLowerCase()===name);if(p){this.jitter=p.jitter;this.backoff_first_ms=p.firstMs;this.backoff_max_ms=p.maxMs;this.backoff_factor=p.factor;}this.backoff_preset=name;},
      onScriptFileDrop(e){this.script_drag_over=false;this._readScriptFile(e.dataTransfer?.files?.[0]);},onScriptFileChange(e){this._readScriptFile(e.target?.files?.[0]);},_readScriptFile(f){if(!f)return;if(f.size>1024*1024){this.error='Script file must be at most 1 MiB.';return;}const reader=new FileReader();reader.onload=()=>{this.script_body=String(reader.result || '');};reader.onerror=()=>{this.error='Could not read script file.';};reader.readAsText(f);}
    };
  }
  root.soltiSpecBuilder=builder;
  if(typeof module!=='undefined')module.exports={builder,argv,labels};
})(typeof window==='undefined'?globalThis:window);
