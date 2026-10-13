package agent

// Private file IPC carries only explicitly authorized continuation requests.
// Native plugins validate identity/current turn again and prepare an ack before
// submitting. A crash or lost response never causes the plugin to submit twice.
const liveBridgeJS = `
const liveStates=new Map();let liveChain=Promise.resolve(),liveBusy=false;
const liveMarker=process.env.CODEGATE_CONVERSATION_MARKER;
const liveWrite=(path,value)=>{liveChain=liveChain.then(async()=>{const temp=path+'.tmp';await writeFile(temp,JSON.stringify(value),{mode:0o600});await rename(temp,path)}).catch(()=>{});return liveChain};
const livePublish=async(value)=>{if(!liveMarker||!value?.id||!value?.turn_id)return;liveStates.set(value.id,value);await liveWrite(liveMarker+'.live.'+value.id+'.json',value)};
const liveCode=(code,status)=>{const c=String(code??'').toLowerCase();if(['authentication_error','invalid_request_error','insufficient_quota','account_quota','permission_error','billing_error'].includes(c))return 'other';if(['quota','usage_limit_reached','usage_limit_exceeded','usageLimitExceeded'.toLowerCase()].includes(c))return 'usageLimitExceeded';if(status===429||c==='rate_limit'||c==='rate_limit_error'||c==='rate_limit_exceeded')return 'rateLimitExceeded';if(status===408||status>=500&&status<=599||['network','network_error','connection_error','timeout','server_error','overloaded_error'].includes(c))return 'serverOverloaded';return 'other'};
const liveReset=(headers,body)=>{let value=headers?.['retry-after']??headers?.['Retry-After'];let at=0;if(value!=null){const seconds=Number(value);at=Number.isFinite(seconds)?Date.now()+seconds*1000:Date.parse(value)}else{value=body?.error?.resets_at??body?.resets_at??body?.resetsAt;if(value!=null){const n=Number(value);at=Number.isFinite(n)?(n<1e12?n*1000:n):Date.parse(value)}}return Number.isFinite(at)&&at>Date.now()&&at<Date.now()+30*86400000?at:undefined};
const livePoll=(execute)=>{if(!liveMarker)return;const timer=setInterval(async()=>{if(liveBusy)return;liveBusy=true;try{const r=JSON.parse(await readFile(liveMarker+'.request.json','utf8'));if(!r.id||!r.native_id||r.deadline<Date.now())return;let ack;try{ack=JSON.parse(await readFile(liveMarker+'.ack.json','utf8'))}catch{}if(ack?.id===r.id)return;const s=liveStates.get(r.native_id);if(!s||s.turn_id!==r.expected_turn||r.action==='turn/start'&&s.state!=='failed')return await liveWrite(liveMarker+'.ack.json',{id:r.id,state:'rejected'});await liveWrite(liveMarker+'.ack.json',{id:r.id,state:'preparing'});const turn=await execute(r,s);await liveWrite(liveMarker+'.ack.json',{id:r.id,state:'ok',turn_id:turn})}catch{}finally{liveBusy=false}},100);timer.unref?.();return timer};
`
const openCodeLiveSetupJS = `

// Newer OpenCode emits session.next events instead of some legacy completion
// notifications. Read native status/history as a compatibility backstop.
let liveStatusBusy=false;const liveStatusTimer=setInterval(async()=>{if(liveStatusBusy)return;liveStatusBusy=true;try{const status=await client.session.status();if(status.error)return;for(const [id,s]of liveStates){if(!['inProgress','failed'].includes(s.state))continue;const type=status.data?.[id]?.type??'idle';if(type!=='idle'){if(s.state!=='inProgress')await livePublish({...s,state:'inProgress'});continue}const messages=await client.session.messages({path:{id},query:{limit:5}});if(messages.error)continue;const info=(messages.data??[]).map(row=>row.info).filter(info=>info?.role==='assistant'&&info.parentID===s.turn_id&&info.time?.completed).at(-1);if(!info)continue;let next={...s,model:{providerID:info.providerID,modelID:info.modelID},agent:info.agent??info.mode,state:info.error?'failed':'completed'};if(info.error){let body={};try{body=JSON.parse(info.error.data?.responseBody??'{}')}catch{}next.code=liveCode(body.error?.type??body.error?.code??body.code,info.error.data?.statusCode);next.reset_at=s.reset_at??liveReset(info.error.data?.responseHeaders,body)}else delete next.code;await livePublish(next)}}catch{}finally{liveStatusBusy=false}},1000);liveStatusTimer.unref?.();
livePoll(async(r,s)=>{
 const state=await client.session.status();if(state.error)throw Error('status unavailable');const type=state.data?.[r.native_id]?.type??'idle';
 if(r.action==='turn/interrupt'){if(type!=='idle')await client.session.abort({path:{id:r.native_id}});return s.turn_id}
 if(type!=='idle')throw Error('native session busy');
 const result=await client.session.promptAsync({path:{id:r.native_id},body:{model:s.model,agent:s.agent,parts:[{type:'text',text:r.prompt}]}});if(result.error)throw Error('native rejected');
 // Let OpenCode mint its time-ordered ID. Arbitrary msg_ IDs can break
 // history ordering even when the HTTP schema accepts them.
 while(Date.now()<r.deadline){const messages=await client.session.messages({path:{id:r.native_id},query:{limit:5}});if(messages.error)throw Error('native ack unavailable');for(const row of (messages.data??[])){if(row.info?.role==='user'&&row.info.id!==r.expected_turn&&row.parts?.some(p=>p.type==='text'&&p.text===r.prompt))return row.info.id}await new Promise(r=>setTimeout(r,25))}throw Error('native message ack timed out')

});
`
const openCodeLiveEventJS = `
  const p=event.properties??{},m=p.info;let id=p.sessionID??m?.sessionID;
  if(id){let s=liveStates.get(id);
   if(event.type==='message.updated'&&m?.role==='user'){
    const owned=await client.session.get({path:{id}});if(owned.data?.directory===directory&&!owned.data?.parentID)s={id,cwd:directory,turn_id:m.id,state:'inProgress',model:m.model,agent:m.agent,at:Date.now()};else s=undefined;
   }else if(event.type==='message.updated'&&m?.role==='assistant'&&s){
    s={...s,model:{providerID:m.providerID,modelID:m.modelID},agent:m.mode??m.agent};
    if(m.error){let body={};try{body=JSON.parse(m.error.data?.responseBody??'{}')}catch{}s.at=s.code?s.at:Date.now();s.code=liveCode(body.error?.type??body.error?.code??body.code,m.error.data?.statusCode);s.state='failed';s.reset_at=liveReset(m.error.data?.responseHeaders,body)}
   }else if(event.type==='session.error'&&s){let body={};try{body=JSON.parse(p.error?.data?.responseBody??'{}')}catch{}s={...s,code:liveCode(body.error?.type??body.error?.code??body.code,p.error?.data?.statusCode),state:'failed',reset_at:liveReset(p.error?.data?.responseHeaders,body),at:Date.now()}}
   else if(event.type==='session.status'&&s){if(p.status?.type==='idle')s={...s,state:s.code?'failed':'completed'};else s={...s,state:'inProgress'}}
   else if(event.type==='session.idle'&&s)s={...s,state:s.code?'failed':'completed'};
   if(s)await livePublish(s);
  }
`
const dshLiveSetupJS = `
livePoll(async(r,s)=>{
 const agents=ctx.get('agents')??ctx.agents;const agent=agents?.get?.(r.native_id);if(!agent)throw Error('native agent absent');
 if(r.action==='turn/interrupt'){agent.cancel({kind:'user'});return s.turn_id}
 if(agent.status!=='idle'||typeof agent.followup!=='function')throw Error('native agent busy or unsupported');
 agent.followup(Object.freeze({id:crypto.randomUUID(),role:'user',content:Object.freeze([Object.freeze({type:'text',text:r.prompt})]),source:Object.freeze({kind:'user'})}));return String(Number(s.turn_id)+1)
});
ctx.on('session/event',async(session,event)=>{
 if(!session?.header||(session.header.delegationDepth??0)>0||session.header.cwd!==process.cwd())return;
 if(event.type==='turn/start')await livePublish({id:session.id,cwd:session.header.cwd,turn_id:String(event.data.turn),state:'inProgress',at:Date.now()});
 if(event.type==='turn/end'){const reason=event.data.reason;const error=reason?.error;await livePublish({id:session.id,cwd:session.header.cwd,turn_id:String(event.data.turn),state:reason?.kind==='error'?'failed':reason?.kind==='completed'?'completed':'interrupted',code:error?liveCode(error.code,error.status):undefined,reset_at:error?.providerRetryAfterMs>0?Date.now()+Math.min(error.providerRetryAfterMs,30*86400000):undefined,at:Date.now()})}
});
`
