// Temporary DevTools diagnostic. Does not change request values or model selection.
(() => {
  if (globalThis.__officeEffortCapture) return 'capture already installed';
  const stringify = JSON.stringify, parse = JSON.parse;
  const state = { records: [], restorers: [] };
  const exact = /^(agentModelType|agentOverride|selectedModel|modelId|modelName|modelAlias|tone|reasoningEffort|reasoning_effort|reasoningLevel|defaultReasoningLevel|supportedReasoningLevels|reasoningLevels|allowedReasoningLevels|effort|budget_tokens|thinkingBudget|thinkingEnabled|max_tokens|maxTokens|input_tokens|output_tokens|reasoning_tokens|completion_tokens|prompt_tokens|total_tokens|messageAnnotationType|typeName|contractVersion|selectorEnabled)$/i;
  const marker = /ClaudeSlot|Claude_Opus|GetModelContract|reason|think|budget|ExcelAgent/i;
  let busy = false;
  const inspect = (direction, data, endpoint = '') => {
    if (busy || state.records.length >= 500) return;
    busy = true;
    try {
      if (typeof data !== 'string' || !marker.test(data)) return;
      for (const part of data.split('\u001e')) {
        let obj;
        try { obj = parse(part); } catch { continue; }
        const fields = [], signals = new Set(), seen = new WeakSet(), candidateKeys = [], bodyKeys = [];
        let visits = 0;
        const walk = (x, path, depth) => {
          if (++visits > 30000 || depth > 22 || x == null) return;
          if (typeof x === 'string') {
            if (/^[\[{]/.test(x)) {
              try { walk(parse(x), path + '.<json>', depth + 1); } catch {}
            }
            if (/^AugLoop_[A-Za-z0-9_]+$/.test(x)) signals.add(x);
            return;
          }
          if (typeof x !== 'object' || seen.has(x)) return;
          seen.add(x);
          for (const [key, value] of Object.entries(x)) {
            const p = path + '.' + key;
            if (/reason|think|effort|budget/i.test(key)) candidateKeys.push({path:p,type:typeof value});
            if (key === 'body' && value && typeof value === 'object') bodyKeys.push({path:p,keys:Object.keys(value)});
            if (exact.test(key) && (value == null || typeof value === 'boolean' || typeof value === 'number' || (typeof value === 'string' && value.length < 100) || (Array.isArray(value) && value.every(v => ['string','number'].includes(typeof v) && String(v).length < 40)))) fields.push({path:p,value});
            walk(value, p, depth + 1);
          }
        };
        walk(obj, '$', 0);
        if (fields.length || signals.size || candidateKeys.length) state.records.push({direction,endpoint,bytes:part.length,fields,signals:[...signals],candidateKeys,bodyKeys,truncated:visits>30000});
      }
    } finally { busy = false; }
  };
  const endpoint = value => {try { const u = new URL(value, location.href); return u.origin + u.pathname; } catch { return ''; }};
  JSON.stringify = function(...args) { const result = Reflect.apply(stringify, this, args); try { inspect('serialize', result); } catch {} return result; };
  JSON.parse = function(...args) { const result = Reflect.apply(parse, this, args); try { inspect('parse', args[0]); } catch {} return result; };
  state.restorers.push(() => { JSON.stringify=stringify; JSON.parse=parse; });
  const send = WebSocket.prototype.send;
  const sockets = new Map();
  WebSocket.prototype.send = function(data) {
    if (!sockets.has(this)) { const listener = event => {try {inspect('ws-receive',event.data,endpoint(this.url));}catch{}}; this.addEventListener('message',listener); sockets.set(this,listener); }
    try {inspect('ws-send',data,endpoint(this.url));}catch{}
    return Reflect.apply(send,this,[data]);
  };
  state.restorers.push(() => { WebSocket.prototype.send=send; for(const [socket,listener] of sockets) socket.removeEventListener('message',listener); });
  state.stop = () => { for(const restore of state.restorers) restore(); return 'capture stopped'; };
  state.summary = () => stringify(state.records);
  globalThis.__officeEffortCapture=state;
  return 'capture installed: serialization + WebSocket; no request values changed';
})()
