// Optional development-only real Chrome oracle. Never used by the Go server.
import { spawn } from 'node:child_process';
import { once } from 'node:events';
const [chrome, url, profile] = process.argv.slice(2);
const child = spawn(chrome, ['--headless', '--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage', '--no-first-run', '--no-default-browser-check', '--disable-background-networking', '--remote-debugging-port=0', `--user-data-dir=${profile}`, 'about:blank'], { stdio: ['ignore', 'ignore', 'pipe'], detached: true });
let ws;
const killChrome = () => { try { process.kill(-child.pid,'SIGKILL'); } catch {} };
const timer = setTimeout(() => { killChrome(); console.error('browser timeout'); process.exitCode=1; if(ws) ws.close(); }, 65000);
try {
 const endpoint = await new Promise((resolve, reject) => {
  let text = '';
  child.stderr.on('data', chunk => { text += chunk; const m = text.match(/DevTools listening on (ws:\/\/[^\s]+)/); if(m) resolve(m[1]); });
  child.once('error', reject); child.once('exit', code => reject(new Error(`Chrome exited ${code}: ${text}`)));
 });
 const target = await fetch(`http://${new URL(endpoint).host}/json/new?about:blank`, { method: 'PUT' }).then(r=>r.json());
 ws = new WebSocket(target.webSocketDebuggerUrl); await once(ws,'open');
 let seq=0; const pending=new Map(); const errors=[], outbound=[];
 const call=(method,params={})=>new Promise((resolve,reject)=>{const id=++seq;pending.set(id,{resolve,reject});ws.send(JSON.stringify({id,method,params}));});
 ws.addEventListener('message',event=>{const m=JSON.parse(event.data);if(m.id){const p=pending.get(m.id);pending.delete(m.id);if(p){if(m.error)p.reject(new Error(JSON.stringify(m.error)));else p.resolve(m.result);}return;}
  if(m.method==='Runtime.exceptionThrown'){errors.push(m.params.exceptionDetails);console.error('EXCEPTION',JSON.stringify(m.params.exceptionDetails));}
  if(m.method==='Runtime.consoleAPICalled')console.error('CONSOLE',JSON.stringify(m.params.args.map(a=>a.value||a.description)));
  if(m.method==='Network.responseReceived' && /v2\/(challenge|redeem|validate)/.test(m.params.response.url))console.error('RESPONSE',m.params.response.url,m.params.response.status);
  if(m.method==='Fetch.requestPaused'){
   const u=new URL(m.params.request.url); const allowed=u.origin===new URL(url).origin||['data:','blob:'].includes(u.protocol);
   if(!allowed)outbound.push(m.params.request.url);
   void call(allowed?'Fetch.continueRequest':'Fetch.failRequest',allowed?{requestId:m.params.requestId}:{requestId:m.params.requestId,errorReason:'BlockedByClient'}).catch(()=>{});
  }
 });
 await call('Page.enable');await call('Runtime.enable');await call('Network.enable');await call('Page.addScriptToEvaluateOnNewDocument',{source:'window.CAP_DEBUG=true;'});await call('Fetch.enable',{patterns:[{urlPattern:'*'}]});await call('Page.navigate',{url});
 const result=await call('Runtime.evaluate',{expression:`new Promise((resolve,reject)=>{const end=Date.now()+40000;setTimeout(()=>reject(new Error('solve timeout '+document.body.innerText)),45000);const check=()=>{const w=document.querySelector('cap-widget');if(w&&typeof w.solve==='function'){w.addEventListener('solve',e=>resolve({solved:true,token:!!e.detail.token}),{once:true});w.addEventListener('error',e=>reject(new Error('widget error '+JSON.stringify(e.detail))),{once:true});w.solve();return;}if(Date.now()>end){reject(new Error('widget unavailable: '+document.body.innerText));return;}setTimeout(check,50);};check();})`,awaitPromise:true,returnByValue:true});
 if(result.exceptionDetails)throw new Error(JSON.stringify(result.exceptionDetails));
 // solve event precedes SamWaf's final validation and cookie issuance.
 let verified=false;
 for(let i=0;i<150;i++){
  const r=await call('Runtime.evaluate',{expression:`document.body.innerText.includes('E2E_ORIGIN_REACHED')`,returnByValue:true}).catch(()=>({}));
  if(r.result?.value){verified=true;break;}
  await new Promise(r=>setTimeout(r,100));
 }
 if(!verified) {const state=await call('Runtime.evaluate',{expression:'document.body.innerText',returnByValue:true});throw new Error('origin not reached '+JSON.stringify({state,errors,outbound}));}
 if(outbound.length)throw new Error('unexpected external dependency '+JSON.stringify(outbound));
 console.log(JSON.stringify({verified,externalRequests:outbound,exceptions:errors}));
} finally {clearTimeout(timer); if(ws)ws.close();killChrome();if(child.exitCode===null)await once(child,'exit').catch(()=>{});}
