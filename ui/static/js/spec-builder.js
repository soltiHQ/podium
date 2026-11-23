(function (root) {
  'use strict';
  function shortFingerprint(fp) { return String(fp || '').slice(0, 12); }
  function labels(rows) { const out={}; for (const r of rows) { const k=r.key.trim(); if (!k) { if(r.value) throw new Error('A label needs a key.'); continue; } if(Object.hasOwn(out,k)) throw new Error('Duplicate key: '+k); Object.defineProperty(out,k,{value:r.value,enumerable:true,writable:true,configurable:true}); } return out; }
  function typeKey(apiVersion, kind) { return apiVersion+' '+kind; }
  function plural(n, word) { return n+' '+word+(n===1?'':'s'); }

  // The workload types an agent declares, by type, with the fingerprint of the
  // first runner that declares each: the runner the agent routes the type to.
  function declaredTypes(agent) {
    const out=new Map();
    for (const r of (agent && agent.capabilities) || []) for (const w of r.workloads || []) {
      const api=w.group ? w.group+'/'+w.version : w.version;
      const key=typeKey(api,w.kind);
      if (!out.has(key)) out.set(key,{api_version:api,kind:w.kind,fingerprint:w.schema_fingerprint || ''});
    }
    return out;
  }

  // ---- A form built from the JSON Schema an agent published for a type ----

  const CLS={
    label:'block text-[11px] uppercase tracking-[0.05em] text-muted mb-1',
    help:'mt-1 text-xs text-muted',
    input:'block w-full h-11 px-4 rounded-[var(--r-xs)] bg-card text-fg placeholder:text-muted-strong outline-none transition-colors border border-input hover:border-border focus:ring-2 focus:ring-primary/25 focus:border-primary',
    select:'block w-full h-11 px-4 rounded-[var(--r-xs)] bg-card text-fg outline-none transition-colors border border-input hover:border-border focus:ring-2 focus:ring-primary/25 focus:border-primary',
    textarea:'block w-full px-4 py-3 rounded-[var(--r-xs)] bg-card text-fg text-sm font-mono border border-input hover:border-border focus:ring-2 focus:ring-primary/25 focus:border-primary outline-none transition-colors resize-y',
    group:'space-y-3 border-l border-border pl-3',
    add:'text-xs text-primary hover:text-primary/80 transition-colors',
    remove:'shrink-0 w-7 h-7 flex items-center justify-center rounded text-muted hover:text-danger transition-colors',
    check:'w-4 h-4 rounded',
  };
  function el(tag, cls, text) { const e=document.createElement(tag); if (cls) e.className=cls; if (text!=null) e.textContent=text; return e; }
  function humanize(name) { const s=String(name || '').replace(/([a-z0-9])([A-Z])/g,'$1 $2').replace(/[_-]+/g,' ').trim().toLowerCase(); return s ? s[0].toUpperCase()+s.slice(1) : s; }
  function b64encode(text) { const bytes=new TextEncoder().encode(text); let s=''; for (const b of bytes) s+=String.fromCharCode(b); return btoa(s); }
  function b64decode(b64) { const s=atob(b64); const bytes=new Uint8Array(s.length); for (let i=0;i<s.length;i++) bytes[i]=s.charCodeAt(i); return new TextDecoder().decode(bytes); }

  // deref follows $ref into the document; keywords beside the $ref win.
  function deref(rootSchema, s) {
    for (let n=0; s && typeof s==='object' && typeof s.$ref==='string' && n<32; n++) {
      const m=/^#\/(\$defs|definitions)\/(.+)$/.exec(s.$ref);
      const name=m ? m[2].replace(/~1/g,'/').replace(/~0/g,'~') : '';
      const target=m && rootSchema[m[1]] ? rootSchema[m[1]][name] : null;
      if (!target) return {};
      const rest=Object.assign({},s); delete rest.$ref;
      s=Object.assign({},target,rest);
    }
    return s && typeof s==='object' ? s : {};
  }
  // nullable returns the schema inside an "X or null", or null.
  function nullable(rootSchema, s) {
    for (const k of ['anyOf','oneOf']) if (Array.isArray(s[k])) {
      const rest=s[k].filter(b=>deref(rootSchema,b).type!=='null');
      if (rest.length===1 && rest.length<s[k].length) { const inner=deref(rootSchema,rest[0]); return Object.assign({},inner,{description:s.description || inner.description, default:s.default!==undefined ? s.default : inner.default}); }
    }
    if (Array.isArray(s.type) && s.type.includes('null')) { const t=s.type.filter(x=>x!=='null'); return Object.assign({},s,{type:t.length===1 ? t[0] : t}); }
    return null;
  }

  function field(meta, s, control, optional) {
    const wrap=el('div');
    if (meta.name) wrap.appendChild(el('label',CLS.label,humanize(meta.name)+(optional ? '' : ' *')));
    wrap.appendChild(control);
    if (s.description) wrap.appendChild(el('p',CLS.help,s.description));
    return wrap;
  }
  function required(meta, label) { return [(humanize(meta.name) || label)+' is required']; }

  function build(ctx, schema, init, meta) {
    let s=deref(ctx.root,schema);
    let optional=!meta.required;
    const inner=nullable(ctx.root,s);
    if (inner) { s=inner; optional=true; }
    if (Array.isArray(s.allOf) && !s.type && !s.properties) {
      const base=s.allOf.map(b=>deref(ctx.root,b)).find(b=>b.type || b.properties);
      if (base) s=Object.assign({},base,{description:s.description || base.description});
    }
    const variants=Array.isArray(s.oneOf) ? s.oneOf : Array.isArray(s.anyOf) ? s.anyOf : null;
    if (variants && !s.properties) return buildUnion(ctx,s,variants.map(v=>deref(ctx.root,v)),init,meta,optional);
    if (Array.isArray(s.enum)) return buildSelect(ctx,s,s.enum.map(v=>({value:v,label:String(v)})),init,meta,optional);
    if (s.const!==undefined) return {el:el('div'),get:()=>s.const,check:()=>[]};
    const type=Array.isArray(s.type) ? s.type[0] : s.type;
    if (type==='object') {
      if (s.properties) return buildObject(ctx,s,init,meta,optional);
      if (s.additionalProperties && typeof s.additionalProperties==='object') return buildMap(ctx,s,init,meta,optional);
      return buildJson(ctx,s,init,meta,optional);
    }
    if (type==='array') return buildArray(ctx,s,init,meta,optional);
    if (type==='string') return buildString(ctx,s,init,meta,optional);
    if (type==='integer' || type==='number') return buildNumber(ctx,s,init,meta,optional,type==='integer');
    if (type==='boolean') return buildBool(ctx,s,init,meta);
    return buildJson(ctx,s,init,meta,optional);
  }

  function buildString(ctx, s, init, meta, optional) {
    const b64=s.contentEncoding==='base64';
    const input=el(b64 ? 'textarea' : 'input',b64 ? CLS.textarea : CLS.input);
    if (b64) input.rows=6; else input.type='text';
    let v=init!=null ? String(init) : s.default!=null ? String(s.default) : '';
    if (b64 && v) { try { v=b64decode(v); } catch (e) { /* keep the raw text */ } }
    input.value=v;
    input.addEventListener('input',ctx.onChange);
    return {el:field(meta,s,input,optional),
      get:()=>{ const t=input.value; if (t==='' && optional) return undefined; return b64 ? b64encode(t) : t; },
      check:()=>(!optional && input.value.trim()==='') ? required(meta,'value') : []};
  }

  function buildNumber(ctx, s, init, meta, optional, integer) {
    const input=el('input',CLS.input); input.type='number'; input.step=integer ? '1' : 'any';
    const v=init!=null ? init : s.default;
    input.value=v!=null ? String(v) : '';
    input.addEventListener('input',ctx.onChange);
    return {el:field(meta,s,input,optional),
      get:()=>input.value==='' ? (optional ? undefined : null) : Number(input.value),
      check:()=>{ if (input.value==='') return optional ? [] : required(meta,'number'); const n=Number(input.value); if (!Number.isFinite(n) || (integer && !Number.isInteger(n))) return [(humanize(meta.name) || 'value')+' must be '+(integer ? 'an integer' : 'a number')]; return []; }};
  }

  function buildBool(ctx, s, init, meta) {
    const row=el('label','flex items-center gap-2 text-sm text-fg');
    const box=el('input',CLS.check); box.type='checkbox';
    box.checked=init!=null ? !!init : !!s.default;
    box.addEventListener('change',ctx.onChange);
    row.appendChild(box); row.appendChild(el('span','',humanize(meta.name) || 'enabled'));
    const wrap=el('div'); wrap.appendChild(row);
    if (s.description) wrap.appendChild(el('p',CLS.help,s.description));
    return {el:wrap,get:()=>box.checked,check:()=>[]};
  }

  function buildSelect(ctx, s, options, init, meta, optional) {
    const select=el('select',CLS.select);
    if (optional) select.appendChild(el('option','','—'));
    options.forEach((o,i)=>{ const opt=el('option','',o.label); opt.value=String(i); if (o.title) opt.title=o.title; select.appendChild(opt); });
    const start=init!==undefined ? init : s.default;
    const idx=options.findIndex(o=>o.value===start);
    select.value=idx>=0 ? String(idx) : optional ? '' : '0';
    if (!optional && idx<0) select.selectedIndex=0;
    select.addEventListener('change',ctx.onChange);
    return {el:field(meta,s,select,optional),
      get:()=>select.value==='' || !options[Number(select.value)] ? undefined : options[Number(select.value)].value,
      check:()=>(!optional && (select.value==='' || !options[Number(select.value)])) ? required(meta,'choice') : []};
  }

  function buildUnion(ctx, s, variants, init, meta, optional) {
    if (variants.every(v=>v.const!==undefined || (Array.isArray(v.enum) && v.enum.length===1)))
      return buildSelect(ctx,s,variants.map(v=>{ const value=v.const!==undefined ? v.const : v.enum[0]; return {value,label:String(value),title:v.description}; }),init,meta,optional);
    const tagged=variants.every(v=>v.properties && Object.keys(v.properties).length===1);
    if (!tagged) return buildJson(ctx,s,init,meta,optional);
    const tags=variants.map(v=>Object.keys(v.properties)[0]);
    const wrap=el('div','space-y-2');
    const select=el('select',CLS.select);
    if (optional) select.appendChild(el('option','','—'));
    tags.forEach((t,i)=>{ const opt=el('option','',humanize(t)+(variants[i].description ? ' — '+variants[i].description : '')); opt.value=String(i); select.appendChild(opt); });
    const body=el('div',CLS.group);
    wrap.appendChild(select); wrap.appendChild(body);
    let node=null;
    const startTag=init && typeof init==='object' ? Object.keys(init)[0] : undefined;
    function show(i, value) {
      body.replaceChildren(); node=null;
      if (i<0) { body.hidden=true; return; }
      body.hidden=false;
      node=build(ctx,variants[i].properties[tags[i]],value,{name:'',required:true});
      body.appendChild(node.el);
    }
    const first=tags.indexOf(startTag);
    select.value=first>=0 ? String(first) : optional ? '' : '0';
    show(select.value==='' ? -1 : Number(select.value),first>=0 ? init[startTag] : undefined);
    select.addEventListener('change',()=>{ show(select.value==='' ? -1 : Number(select.value),undefined); ctx.onChange(); });
    return {el:field(meta,s,wrap,optional),
      get:()=>{ if (!node) return undefined; const v=node.get(); return {[tags[Number(select.value)]]:v===undefined ? {} : v}; },
      check:()=>node ? node.check() : optional ? [] : required(meta,'choice')};
  }

  function buildObject(ctx, s, init, meta, optional) {
    const req=new Set(s.required || []);
    const names=Object.keys(s.properties);
    names.sort((a,b)=>(req.has(a) ? 0 : 1)-(req.has(b) ? 0 : 1));
    const box=el(meta.name ? 'fieldset' : 'div',meta.name ? CLS.group : 'space-y-4');
    const children=names.map(name=>{ const c=build(ctx,s.properties[name],init && typeof init==='object' ? init[name] : undefined,{name,required:req.has(name)}); box.appendChild(c.el); return [name,c]; });
    function collect() { const out={}; for (const [name,c] of children) { const v=c.get(); if (v!==undefined) out[name]=v; } return out; }
    if (!meta.name || !optional) {
      const wrap=meta.name ? field(meta,s,box,false) : box;
      return {el:wrap,get:collect,check:()=>children.flatMap(([,c])=>c.check())};
    }
    // An optional object is left out unless it is turned on.
    const wrap=el('div');
    const row=el('label','flex items-center gap-2 text-sm text-fg');
    const on=el('input',CLS.check); on.type='checkbox'; on.checked=init!=null;
    row.appendChild(on); row.appendChild(el('span','',humanize(meta.name)));
    wrap.appendChild(row);
    if (s.description) wrap.appendChild(el('p',CLS.help,s.description));
    wrap.appendChild(box); box.hidden=!on.checked;
    on.addEventListener('change',()=>{ box.hidden=!on.checked; ctx.onChange(); });
    return {el:wrap,get:()=>on.checked ? collect() : undefined,check:()=>on.checked ? children.flatMap(([,c])=>c.check()) : []};
  }

  function rowList(ctx, makeRow, initial) {
    const list=el('div','space-y-2');
    const rows=[];
    function add(value) {
      const line=el('div','flex items-start gap-2');
      const cell=el('div','flex-1 min-w-0');
      const node=makeRow(value);
      cell.appendChild(node.el);
      const del=el('button',CLS.remove,'×'); del.type='button';
      const entry={line,node};
      del.addEventListener('click',()=>{ rows.splice(rows.indexOf(entry),1); line.remove(); ctx.onChange(); });
      line.appendChild(cell); line.appendChild(del);
      list.appendChild(line); rows.push(entry);
    }
    for (const v of initial) add(v);
    const plus=el('button',CLS.add,'+ Add'); plus.type='button';
    plus.addEventListener('click',()=>{ add(undefined); ctx.onChange(); });
    const box=el('div','space-y-2'); box.appendChild(list); box.appendChild(plus);
    return {box,rows};
  }

  function buildArray(ctx, s, init, meta, optional) {
    const items=s.items || {};
    const {box,rows}=rowList(ctx,value=>build(ctx,items,value,{name:'',required:true}),Array.isArray(init) ? init : Array.isArray(s.default) ? s.default : []);
    return {el:field(meta,s,box,optional),
      get:()=>{ const out=rows.map(r=>r.node.get()); return out.length===0 && optional ? undefined : out; },
      check:()=>{ const p=rows.flatMap(r=>r.node.check()); if (s.minItems && rows.length<s.minItems) p.push((humanize(meta.name) || 'list')+' needs at least '+plural(s.minItems,'item')); return p; }};
  }

  function buildMap(ctx, s, init, meta, optional) {
    const valueSchema=s.additionalProperties;
    const entries=init && typeof init==='object' ? Object.entries(init) : [];
    const {box,rows}=rowList(ctx,pair=>{
      const line=el('div','flex items-center gap-2');
      const k=el('input',CLS.input); k.type='text'; k.placeholder='key'; k.value=pair ? pair[0] : '';
      k.addEventListener('input',ctx.onChange);
      const v=build(ctx,valueSchema,pair ? pair[1] : undefined,{name:'',required:true});
      line.appendChild(k); line.appendChild(v.el);
      return {el:line,get:()=>[k.value,v.get()],check:()=>k.value.trim()==='' ? ['a key is required'] : v.check()};
    },entries);
    return {el:field(meta,s,box,optional),
      get:()=>{ const out={}; for (const r of rows) { const [k,v]=r.node.get(); if (k!=='') out[k]=v; } return Object.keys(out).length===0 && optional ? undefined : out; },
      check:()=>rows.flatMap(r=>r.node.check())};
  }

  // A value the schema leaves open ("any object") is edited as JSON.
  function buildJson(ctx, s, init, meta, optional) {
    const area=el('textarea',CLS.textarea); area.rows=4; area.spellcheck=false;
    area.value=init!==undefined ? JSON.stringify(init,null,2) : optional ? '' : '{}';
    area.addEventListener('input',ctx.onChange);
    function parse() { return area.value.trim()==='' ? undefined : JSON.parse(area.value); }
    return {el:field(meta,s,area,optional),
      get:()=>{ try { const v=parse(); return v===undefined && !optional ? {} : v; } catch (e) { return undefined; } },
      check:()=>{ try { parse(); return []; } catch (e) { return [(humanize(meta.name) || 'value')+' is not valid JSON']; } }};
  }

  function renderSchemaForm(container, schema, initial, onChange) {
    const ctx={root:schema,onChange:onChange || function () {}};
    const node=build(ctx,schema,initial,{name:'',required:true});
    container.replaceChildren(node.el);
    return Object.freeze({value:()=>node.get(),problems:()=>node.check()});
  }

  // ---- The spec builder: agents first, then a type they share, then its form ----

  function builder(seed, presets, endpoint, typesEndpoint) {
    const base={name:'',slot:'',type_key:'',fingerprint:'',group_key:'',types:[],types_error:'',problems:[],timeout_ms:30000,restart_type:'never',interval_ms:0,backoff_preset:'standard',jitter:'none',backoff_first_ms:1000,backoff_max_ms:5000,backoff_factor:2,loaded_version:0,agents:[],agents_opts:[],agents_info:{},agents_open:false,target_mode:'agents',label_rows:[],runner_label_rows:[],error:'',agent_error:'',schema_error:'',schema_loading:false,form:null,form_rev:0,conflict:false,submitting:false,presets,agents_endpoint:endpoint,types_endpoint:typesEndpoint,seed_workload:null,seed_body:undefined};
    if(seed) {
      for(const k of ['name','slot','timeout_ms','restart_type','interval_ms','jitter','backoff_first_ms','backoff_max_ms','backoff_factor']) if(seed[k]!=null) base[k]=seed[k];
      base.loaded_version=seed.version || 0;
      const w=seed.workload;
      if(w) { base.seed_workload={api_version:w.api_version,kind:w.kind,schema_fingerprint:w.schema_fingerprint}; base.seed_body=w.body || {}; }
      base.agents=(seed.targets || []).slice(); base.label_rows=Object.entries(seed.target_labels || {}).map(([key,value])=>({key,value})); base.runner_label_rows=Object.entries(seed.runner_labels || {}).map(([key,value])=>({key,value})); base.backoff_preset='custom';
      if(base.agents.length===0 && base.label_rows.length>0) base.target_mode='labels';
    }
    return {...base,
      // Every known agent, plus selected ones discovery no longer lists.
      get agentOptions() { return [...new Set([...this.agents_opts,...this.agents])].sort(); },
      agentTypes(id) { const a=this.agents_info[id]; if(!a) return 'not in discovery'; const kinds=[...declaredTypes(a).values()].map(t=>t.kind); return kinds.length ? kinds.join(', ') : 'no workload types'; },
      // Groups of agents that declare one type with one schema, from the catalog.
      get groupList() {
        const out=[];
        for (const t of this.types) for (const f of t.fingerprints || []) if (f.received) out.push({key:typeKey(t.api_version,t.kind)+' '+f.fingerprint,api_version:t.api_version,kind:t.kind,fingerprint:f.fingerprint,agents:(f.agents || []).slice().sort(),label:t.api_version+' '+t.kind+' · '+shortFingerprint(f.fingerprint)+' · '+plural((f.agents || []).length,'agent')});
        return out.sort((a,b)=>a.key.localeCompare(b.key));
      },
      // The types the spec can carry: in agents mode those every selected agent
      // declares with the same schema; in labels mode the chosen group's type.
      get candidates() {
        if (this.target_mode==='labels') {
          const g=this.groupList.find(x=>x.key===this.group_key);
          return g ? [{key:typeKey(g.api_version,g.kind),api_version:g.api_version,kind:g.kind,fingerprint:g.fingerprint,reason:'',label:g.label}] : [];
        }
        if (this.agents.length===0) return [];
        const maps=this.agents.map(id=>this.agents_info[id] ? declaredTypes(this.agents_info[id]) : new Map());
        const keys=new Set(maps.flatMap(m=>[...m.keys()]));
        const out=[];
        for (const key of keys) {
          const ts=maps.map(m=>m.get(key));
          const t=ts.find(Boolean);
          let reason='';
          if (ts.some(x=>!x)) reason='not declared by every selected agent';
          else if (ts.some(x=>!x.fingerprint)) reason='declared without a schema';
          else if (new Set(ts.map(x=>x.fingerprint)).size>1) reason='the selected agents declare different schemas';
          out.push({key,api_version:t.api_version,kind:t.kind,fingerprint:reason ? '' : t.fingerprint,reason,label:t.api_version+' '+t.kind+(reason ? ' — '+reason : ' · '+shortFingerprint(t.fingerprint))});
        }
        return out.sort((a,b)=>(a.reason ? 1 : 0)-(b.reason ? 1 : 0) || a.key.localeCompare(b.key));
      },
      get currentCandidate() { return this.candidates.find(c=>c.key===this.type_key && c.fingerprint) || null; },
      clearForm() { this.type_key=''; this.fingerprint=''; this.form=null; this.schema_error=''; this.form_rev++; if(this.$refs.workload_form) this.$refs.workload_form.replaceChildren(); },
      toggleAgent(id) { this.agents=this.agents.includes(id) ? this.agents.filter(x=>x!==id) : [...this.agents,id]; this.onTargetsChange(); },
      setMode(mode) { if(this.target_mode===mode) return; this.target_mode=mode; this.group_key=''; this.clearForm(); this.onTargetsChange(); },
      pickGroup(key) {
        this.group_key=key;
        const g=this.groupList.find(x=>x.key===key);
        if (g && this.target_mode==='agents') this.agents=g.agents.slice();
        this.onTargetsChange();
      },
      // Keeps the type while the targets still share its schema, picks the only
      // shared type, and clears the form otherwise.
      onTargetsChange() {
        const valid=this.candidates.filter(c=>c.fingerprint);
        const keep=valid.find(c=>c.key===this.type_key);
        if (keep) { if (keep.fingerprint!==this.fingerprint || !this.form) this.selectType(keep.key); return; }
        this.clearForm();
        if (valid.length===1) this.selectType(valid[0].key);
      },
      async selectType(key) {
        this.type_key=key;
        const c=this.currentCandidate;
        this.fingerprint=c ? c.fingerprint : '';
        this.form=null; this.schema_error=''; this.form_rev++;
        const box=this.$refs.workload_form; if (box) box.replaceChildren();
        if (!c) return;
        this.schema_loading=true;
        try {
          const path=c.api_version.split('/').map(encodeURIComponent).join('/')+'/'+encodeURIComponent(c.kind);
          const r=await fetch(this.types_endpoint+'/'+path,{headers:{Accept:'application/json'}});
          if (!r.ok) throw new Error('The schema is unavailable ('+r.status+').');
          const d=await r.json();
          const f=(d.fingerprints || []).find(x=>x.fingerprint===c.fingerprint);
          if (!f || !f.schema) throw new Error('Podium has not received this schema yet. Retry in a moment.');
          if (this.fingerprint!==c.fingerprint || this.type_key!==key) return;
          const schema=typeof f.schema==='string' ? JSON.parse(f.schema) : f.schema;
          const w=this.seed_workload;
          const init=w && typeKey(w.api_version,w.kind)===key && w.schema_fingerprint===c.fingerprint ? this.seed_body : undefined;
          this.form=renderSchemaForm(this.$refs.workload_form,schema,init,()=>{ this.form_rev++; });
          this.form_rev++;
        } catch (e) { this.schema_error=e.message; } finally { this.schema_loading=false; }
      },
      get workloadBody() { void this.form_rev; const v=this.form ? this.form.value() : undefined; return v && typeof v==='object' ? v : {}; },
      get workload() {
        const c=this.currentCandidate;
        return {api_version:c ? c.api_version : '',kind:c ? c.kind : '',schema_fingerprint:this.fingerprint,body:this.workloadBody};
      },
      get targetLabels(){ return labels(this.label_rows); }, get runnerLabels(){return labels(this.runner_label_rows);},
      get createSpec(){return {name:this.name.trim(),slot:this.slot.trim(),workload:this.workload,timeout_ms:Number(this.timeout_ms),restart_type:this.restart_type,interval_ms:this.restart_type==='always'?Number(this.interval_ms):0,jitter:this.jitter,backoff_first_ms:Number(this.backoff_first_ms),backoff_max_ms:Number(this.backoff_max_ms),backoff_factor:Number(this.backoff_factor),version:this.loaded_version,targets:this.target_mode==='agents'?this.agents.slice():[],target_labels:this.target_mode==='labels'?this.targetLabels:{},runner_labels:this.runnerLabels};},
      get previewJSON(){try{return JSON.stringify(this.createSpec,null,2);}catch(e){return e.message;}},
      validate(){if(!this.currentCandidate || !this.fingerprint) throw new Error(this.target_mode==='agents' ? 'Select agents, then a workload type they share.' : 'Select a group of agents with one schema.');if(!this.form) throw new Error('The workload form is not ready.');const fp=this.form.problems();if(fp.length) throw new Error(fp.join('; '));const s=this.createSpec;if(!s.name || !s.slot) throw new Error('Name and slot are required.'); for(const [k,v] of Object.entries(s)) if(typeof v==='number' && !Number.isFinite(v)) throw new Error(k+' must be a finite number.'); if(!Number.isSafeInteger(s.timeout_ms) || s.timeout_ms<=0) throw new Error('Timeout must be a positive integer.'); if(s.backoff_first_ms<=0 || !Number.isSafeInteger(s.backoff_first_ms) || s.backoff_max_ms<s.backoff_first_ms || !Number.isSafeInteger(s.backoff_max_ms) || s.backoff_factor<1) throw new Error('Backoff needs positive integer durations, maximum ≥ first, and factor ≥ 1.'); if(s.restart_type==='always' && (s.interval_ms<=0 || !Number.isSafeInteger(s.interval_ms))) throw new Error('Always restart needs a positive integer interval.'); return s;},
      // Loads the catalog and the agents, then restores an edited spec's type and body.
      async start(){
        await Promise.all([this.loadTypes(),this.loadAgents()]);
        const w=this.seed_workload; if(!w) return;
        const key=typeKey(w.api_version,w.kind);
        if (this.target_mode==='labels') { const g=this.groupList.find(x=>x.key===key+' '+w.schema_fingerprint); if (g) { this.group_key=g.key; this.selectType(key); } else this.schema_error='The saved schema is no longer in the catalog. Pick a group to edit the workload again.'; return; }
        const c=this.candidates.find(x=>x.key===key);
        if (c && c.fingerprint===w.schema_fingerprint) this.selectType(key);
        else this.schema_error=c && c.fingerprint ? 'The selected agents now declare another schema for this type: pick the type again to fill a new form.' : 'The selected agents no longer share this type with one schema.';
      },
      async loadTypes(){this.types_error='';try{const r=await fetch(this.types_endpoint,{headers:{Accept:'application/json'}});if(!r.ok)throw new Error('Workload types unavailable ('+r.status+').');const d=await r.json();this.types=Array.isArray(d)?d:(d.items || []);}catch(e){this.types_error=e.message;}},
      async loadAgents(){this.agent_error='';try{let url=this.agents_endpoint;const info={},items=[];for(let page=0;url && page<100;page++){const r=await fetch(url,{headers:{Accept:'application/json'}});if(!r.ok)throw new Error('Agents unavailable ('+r.status+'). You can still edit existing targets.');const d=await r.json();for(const a of d.items || [])if(!info[a.id]){info[a.id]=a;items.push(a.id);}url=d.next_cursor?this.agents_endpoint+'?cursor='+encodeURIComponent(d.next_cursor):'';}this.agents_info=info;this.agents_opts=items;}catch(e){this.agent_error=e.message;}},
      async submit(endpoint,method){if(this.submitting)return;this.error='';this.problems=[];this.conflict=false;let body;try{body=this.validate();}catch(e){this.error=e.message;return;}this.submitting=true;try{const r=await fetch(endpoint,{method,headers:{'Content-Type':'application/json',Accept:'application/json','HX-Request':'true'},body:JSON.stringify(body)});const redirect=r.headers.get('HX-Redirect');if(r.status===401){root.location.href=redirect || '/login';return;}if(r.ok){if(redirect)root.location.href=redirect;return;}let d={};try{d=await r.json();}catch{}this.conflict=r.status===409;if(r.status===400 && d.message){this.error='The server rejected the spec. Your edits are retained.';this.problems=String(d.message).split('; ').filter(Boolean);}else{this.error=d.message || 'Save failed ('+r.status+'). Your edits are retained.';if(this.conflict)this.error+=' Your edits are retained. Open the latest version in another tab before reconciling changes.';}}catch{this.error='Connection failed. Your edits are retained; retry when the server is available.';}finally{this.submitting=false;}},
      applyPreset(name){const p=this.presets.find(p=>p.label.toLowerCase()===name);if(p){this.jitter=p.jitter;this.backoff_first_ms=p.firstMs;this.backoff_max_ms=p.maxMs;this.backoff_factor=p.factor;}this.backoff_preset=name;}
    };
  }
  root.soltiSpecBuilder=builder;
  root.soltiSchemaForm={render:renderSchemaForm};
  if(typeof module!=='undefined')module.exports={builder,labels,shortFingerprint,declaredTypes};
})(typeof window==='undefined'?globalThis:window);
