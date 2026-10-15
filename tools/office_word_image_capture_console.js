// Temporary local diagnostic: records only image-related protocol structure.
// Does not change requests, read document text, or retain credentials/image bytes.
(() => {
  if (globalThis.__officeImageCapture) return 'image capture already installed';
  const stringify = JSON.stringify, parse = JSON.parse, send = WebSocket.prototype.send, fetch0 = globalThis.fetch;
  const records = [], ids = new Map();
  let busy = false;
  const id = x => { if (!ids.has(x)) ids.set(x, 'ref-' + ids.size); return ids.get(x); };
  const clean = (x, key = '') => {
    if (x == null) return x;
    if (typeof x === 'string') {
      if (/token|authorization|cookie/i.test(key)) return '[redacted]';
      if (/^data:image\//.test(x)) return x.slice(0,x.indexOf(',')+1)+'[image bytes]';
      if (/id$|url$/i.test(key)) return id(x);
      if (/text|prompt|context|content/i.test(key)) return '[text '+x.length+' chars]';
      return x.length<200 ? x : '[long string]';
    }
    if (Array.isArray(x)) return x.map(y=>clean(y,key));
    if (typeof x==='object') return Object.fromEntries(Object.entries(x).map(([k,v])=>[k,clean(v,k)]));
    return x;
  };
  const inspect = (text, kind) => {
    if(busy || typeof text!=='string' || records.length>=60 || !/messageAnnotations|imageUrl|FileBase64|UploadImage/.test(text))return;
    busy=true;
    try { for(const part of text.split('\x1e')) {
      let x;try{x=parse(part);}catch{continue;}
      const p=x.arguments?.[0]??x;
      if(p.message) records.push({kind,payload:clean(p)});
    }} finally{busy=false;}
  };
  JSON.stringify=function(...a){const s=Reflect.apply(stringify,this,a);inspect(s,'serialize');return s;};
  WebSocket.prototype.send=function(s){inspect(s,'websocket');return Reflect.apply(send,this,[s]);};
  globalThis.fetch=async function(input,init){
    const u=String(input?.url??input);
    if(u.includes('UploadFile') && init?.body instanceof FormData){
      const h=new Headers(init.headers);
      records.push({kind:'upload',path:new URL(u,location.href).pathname,gpt:h.get('X-GptId'),variants:h.get('X-Variants'),fields:[...init.body].map(([k,v])=>[k,k==='FileBase64'?String(v).slice(0,40)+'[bytes]':clean(v,k)])});
      const r=await Reflect.apply(fetch0,this,[input,init]);
      r.clone().json().then(x=>records.push({kind:'uploaded',body:clean(x)})).catch(()=>{});return r;
    }
    return Reflect.apply(fetch0,this,[input,init]);
  };
  globalThis.__officeImageCapture={summary:()=>stringify(records),stop:()=>{JSON.stringify=stringify;WebSocket.prototype.send=send;globalThis.fetch=fetch0;return 'restored';}};
  return 'image capture installed';
})()
