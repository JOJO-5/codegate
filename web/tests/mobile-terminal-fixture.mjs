import http from 'node:http';import fs from 'node:fs';import path from 'node:path';
const {WebSocketServer}=await import(process.env.CODEGATE_FIXTURE_WS_MODULE || 'ws');
const root=process.argv[2],uid=n=>`${String(n).padStart(8,'0')}-1111-4111-8111-111111111111`;
const row=(n,name,status,command='codex',bound=true)=>({session_id:uid(n),device_id:'device',name,command,cwd:'/workspace/demo',status,created_at:Date.now()+n,cols:80,rows:24,...(bound?{recovery:{command_id:command,native_id:uid(n+100)}}:{})});
let records=[row(1,'已关闭的原对话','terminated'),row(2,'正在运行的对话','running'),row(3,'另一个运行对话','running'),row(4,'待选择原生历史','terminated','codex',false),row(5,'普通终端','exited','bash',false),row(6,'状态滞后的对话','running'),row(7,'Codex 名称的普通终端','running','cmd.exe',false)];
let failRestore=true,failList=false,failMeta=false,offline=false,restoreCalls=0,stdin=[],signals=[],attachCalls=[],detachCalls=[];
const server=http.createServer(async(req,res)=>{
 const p=new URL(req.url,'http://localhost').pathname;
 const send=(data,status=200)=>{res.writeHead(status,{'Content-Type':'application/json'});res.end(JSON.stringify(data))};
 if(p==='/fixture/state')return send({restoreCalls,stdin,signals,attachCalls,detachCalls});
 if(p.startsWith('/fixture/')){
  const action=p.slice(9);if(action==='restore-ready')failRestore=false;if(action==='restore-fail')failRestore=true;
  if(action==='list-fail')failList=true;if(action==='list-ready')failList=false;
  if(action==='meta-fail')failMeta=true;if(action==='meta-ready')failMeta=false;
  if(action==='offline'){offline=true;for(const c of ws.clients)c.close(1012,'fixture offline')} if(action==='viewer'||action==='controller'){for(const c of ws.clients)c.send(JSON.stringify({v:1,type:'session.role_changed',session_id:uid(2),payload:{session_id:uid(2),role:action,cols:80,rows:24}}))}if(action==='online')offline=false;
  if(action==='close-live'){records=records.map(s=>s.session_id===uid(2)?{...s,status:'terminated'}:s);}
  if(action==='reconnect'){for(const client of ws.clients)client.close(1012,'fixture reconnect')}
  return send({});
 }
 if(p.endsWith('/auth/refresh'))return send({access_token:'fixture',token_type:'Bearer',expires_in:900});
 if(p.endsWith('/me'))return send({id:'fixture',email:'fixture@example.test',role:'user'});
 if(p.endsWith('/preferences'))return send({values:{}});
 if(p.endsWith('/devices'))return send({devices:[{id:'device',name:'隔离验收电脑',online:!offline,platform:'windows',arch:'amd64'}]});
 if(p.startsWith('/api/v1/sessions/')){const found=records.find(s=>s.session_id===p.split('/').at(-1));return failMeta?send({error:{code:'unavailable',message:'读取失败'}},503):found?send(found):send({error:{code:'not_found',message:'会话不存在'}},404)}
 if(p.endsWith('/sessions'))return send({sessions:records});
 if(p.endsWith('/continuations'))return send({tasks:[]});
 if(p.endsWith('/git'))return send({branch:'main',root:'/workspace/demo',files:[]});
 if(p.endsWith('/ws-ticket')&&offline)return send({error:{code:'unavailable',message:'Fixture offline'}},503);
 if(p.endsWith('/ws-ticket'))return send({ticket:'fixture',expires_in:30,protocol:{min:1,max:1}});
 if(p.startsWith('/api/'))return send({});
 let file=path.join(root,p);if(!fs.existsSync(file)||!fs.statSync(file).isFile())file=path.join(root,'index.html');
 res.writeHead(200,{'Content-Type':{'.html':'text/html','.js':'application/javascript','.css':'text/css'}[path.extname(file)]||'application/octet-stream'});fs.createReadStream(file).pipe(res);
});
const ws=new WebSocketServer({server});
ws.on('connection',socket=>socket.on('message',(data,binary)=>{
 if(binary){stdin.push([...Buffer.from(data).subarray(20)]);return}
 const req=JSON.parse(data.toString()),sid=req.payload?.session_id;
 const reply=(type,payload)=>{if(socket.readyState===1)socket.send(JSON.stringify({v:1,type,reply_to:req.request_id,ts:Date.now(),payload}))};
 const error=(code,message)=>reply('error',{code,message});
 if(req.type==='ping')return reply('pong',{});
 if(req.type==='session.list')return offline?error('device_offline','设备离线'):reply('session.list.result',{sessions:records.filter(s=>s.status==='running'&&s.session_id!==uid(6))});
 if(req.type==='session.attach'){
  attachCalls.push(sid);const session=records.find(s=>s.session_id===sid&&s.status==='running'&&s.session_id!==uid(6));
  if(!session)return error('session_not_found','会话已关闭');
  reply('session.attached',{session,role:'controller',seq_from:0,seq_to:0});
  const frame=Buffer.alloc(20+Buffer.byteLength('Codex question: choose an option. Shift+Tab to go back.\r\n'));frame.write('Codex question: choose an option. Shift+Tab to go back.\r\n',20);frame[0]=1;frame[1]=3;frame.writeUInt16BE(1,2);Buffer.from(sid.replaceAll('-',''),'hex').copy(frame,4);socket.send(frame);return;
 }
 if(req.type==='session.detach'){detachCalls.push(sid);return records.find(s=>s.session_id===sid)?.status==='running'?reply('session.detached',{session_id:sid}):error('session_not_found','会话已关闭')}
 if(req.type==='session.resize')return reply('session.resized',{});
 if(req.type==='session.signal'){signals.push(req.payload);return reply('session.info',{session:records.find(s=>s.session_id===sid)})}
 if(req.type==='conversation.list')return failList?error('unavailable','历史读取失败，请重试'):reply('conversation.list.result',{conversations:[{id:uid(200),title:'保存的项目讨论',created_at:1,updated_at:2}]});
 if(req.type==='conversation.restore'){
  restoreCalls++;return setTimeout(()=>{
   if(failRestore)return error('unavailable','设备暂时无法恢复，请重试');
   const source=records.find(s=>s.session_id===sid);
   const session={...source,session_id:uid(10),status:'running',recovery:{command_id:'codex',native_id:source.recovery?.native_id??req.payload.native_id}};
   records=[session,...records.filter(s=>s.session_id!==session.session_id)];reply('session.created',{session});
  },500);
 }
 return error('unavailable','隔离验收：未连接真实执行设备');
}));
server.listen(4198,'127.0.0.1',()=>console.log('isolated mobile terminal fixture ready at 4198'));
