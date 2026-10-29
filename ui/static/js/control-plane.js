(function(){
  'use strict';
  const body=document.body;
  function notify(text){const box=document.getElementById('request-status');if(box){box.hidden=false;box.textContent=text;}}
  document.addEventListener('htmx:beforeSwap',e=>{if(e.detail.target?.querySelector('[data-modal-open="true"]'))e.detail.shouldSwap=false;});
  document.addEventListener('htmx:configRequest',e=>{if(e.detail.verb!=='get')e.detail.headers.Accept='application/json';});
  document.addEventListener('htmx:afterRequest',e=>{
    const d=e.detail,x=d.xhr;if(d.successful||!x)return;
    if(x.status===401){location.href=x.getResponseHeader('HX-Redirect')||'/login';return;}
    let message=x.status?'Request failed ('+x.status+'). Retry when the service is available.':'Connection failed. Retry when the service is available.';
    try{message=JSON.parse(x.responseText).message||message;}catch{}
    notify(message);
    const elt=d.elt,target=d.target;
    if(elt && elt.getAttribute('hx-trigger')?.includes('load') && target && target.isConnected){
      target.replaceChildren();const p=document.createElement('p');p.className='text-sm text-danger';p.textContent=message;target.append(p);
      const button=document.createElement('button');button.type='button';button.className='mt-3 text-sm text-primary';button.textContent='Retry';button.onclick=()=>window.htmx.ajax('GET',elt.getAttribute('hx-get'),{target,swap:'innerHTML'});target.append(button);
    }
  });
  let events;
  const connection=document.getElementById('connection-status');
  if(connection && typeof EventSource!=='undefined'){
    events=new EventSource('/api/v1/events/stream');
    events.onopen=()=>{connection.textContent='Live updates connected';};
    events.onerror=()=>{connection.textContent='Reconnecting · polling remains active';};
    events.onmessage=e=>{if(window.htmx)window.htmx.trigger(body,e.data);};
    window.addEventListener('pagehide',()=>events.close(),{once:true});
  }
  const streams=new Set();
  function state(host,text){const el=host.querySelector('.task-logs-state');if(el)el.textContent=text;}
  function close(host){if(host.__es){host.__es.close();host.__es=null;}streams.delete(host);state(host,'Paused');}
  function decode(raw){try{return new TextDecoder().decode(Uint8Array.from(atob(raw),c=>c.charCodeAt(0)));}catch{return '[invalid log encoding]';}}
  function append(pre,line){const follow=pre.scrollHeight-pre.scrollTop-pre.clientHeight<16;let value=pre.textContent+line+'\n';if(value.length>262144)value='[older output removed; browser retains at most 256 KiB]\n'+value.slice(-245760);pre.textContent=value;if(follow)pre.scrollTop=pre.scrollHeight;}
  function open(host){const pre=host.querySelector('.task-logs-pre'),url=host.dataset.streamUrl;if(!url||!pre||host.__es)return;const es=new EventSource(url);host.__es=es;streams.add(host);state(host,'Connecting…');es.onopen=()=>state(host,'Live · browser keeps up to 256 KiB');es.onerror=()=>state(host,'Reconnecting… collapse to pause');es.onmessage=e=>{let p;try{p=JSON.parse(e.data);}catch{return;}if(p.chunk){append(pre,decode(p.chunk.line||'')+(p.chunk.truncated?' [source truncated]':''));return;}if(p.runStarted){append(pre,'--- generation '+p.runStarted.generation+' / attempt '+p.runStarted.attempt+' started ---');return;}if(p.runFinished){append(pre,'--- generation '+p.runFinished.generation+' / attempt '+p.runFinished.attempt+' finished'+(p.runFinished.exitCode!=null?' exit='+p.runFinished.exitCode:'')+' ---');return;}if(p.lagged)append(pre,'--- missed '+p.lagged.skipped+' events / '+(p.lagged.skippedBytes||0)+' bytes ---');};}
  document.addEventListener('toggle',e=>{if(!(e.target instanceof HTMLDetailsElement))return;const host=e.target.querySelector('[data-stream-url]');if(host){if(e.target.open)open(host);else close(host);}},true);
  new MutationObserver(()=>{for(const host of streams)if(!host.isConnected)close(host);}).observe(body,{subtree:true,childList:true});
  window.addEventListener('pagehide',()=>{for(const host of Array.from(streams))close(host);},{once:true});
})();
