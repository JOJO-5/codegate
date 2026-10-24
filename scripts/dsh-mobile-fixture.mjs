import http from 'node:http';import fs from 'node:fs';import path from 'node:path';import os from 'node:os';import {fileURLToPath} from 'node:url';import {spawn,execFileSync} from 'node:child_process';import {createInterface} from 'node:readline';
const workspace=fileURLToPath(new URL('..',import.meta.url)),port=4190,authority='127.0.0.1:'+port;
const bin=process.env.CODEGATE_DSH_SMOKE;if(!bin)throw Error('Set CODEGATE_DSH_SMOKE to the installed dsh/lib/bin.js');
const fixtureHome=fs.mkdtempSync(path.join(os.tmpdir(),'cg-dsh-mobile-'));
// DSH's first-use workspace otherwise defaults to the account's Documents,
// independently of DSH_HOME. Keep even that default inside the fixture.
fs.writeFileSync(path.join(fixtureHome,'cordis.patch.yml'),'- id: workspace-controller\n  config:\n    documentsDirectory: '+JSON.stringify(fixtureHome)+'\n- id: ui-settings-general\n  config:\n    welcomeNoticeVersion: 2026-09-28.1\n');
const isolatedEnv={...process.env};for(const key of Object.keys(isolatedEnv))if(/^(DSH_|DEEPSEEK_|OPENAI_|ANTHROPIC_|OPENCODE_)/i.test(key))delete isolatedEnv[key];isolatedEnv.DSH_HOME=fixtureHome;
const child=spawn(process.execPath,[bin,'web','--no-open','--port','0','--trusted-host',authority],{cwd:fixtureHome,env:isolatedEnv,windowsHide:true,stdio:['ignore','pipe','ignore']});
let childExited=false;child.once('exit',()=>{childExited=true});
const ready=new Promise((resolve,reject)=>{const lines=createInterface({input:child.stdout});lines.on('line',line=>{if(line.trim().startsWith('dsh web: http://'))resolve(line.trim().slice('dsh web: '.length))});child.once('exit',()=>reject(Error('isolated DSH exited')))});
const raw=await Promise.race([ready,new Promise((_,reject)=>setTimeout(()=>reject(Error('DSH readiness timeout')),40000))]);const upstream=new URL(raw);
const cookie=await new Promise((resolve,reject)=>{http.get(upstream,{headers:{Host:authority}},res=>{res.resume();if(res.statusCode!==303)return reject(Error('bootstrap failed'));resolve((res.headers['set-cookie']??[]).map(s=>s.split(';')[0]).join('; '))}).on('error',reject)});
let mode='raw';const upstreamOptions=req=>({hostname:upstream.hostname,port:upstream.port,path:req.url,method:req.method,headers:{...req.headers,host:authority,cookie,'accept-encoding':req.url==='/'?'identity':req.headers['accept-encoding']??'identity'}});
function shutdown(){try{if(!childExited){if(process.platform==='win32')execFileSync('taskkill',['/pid',String(child.pid),'/t','/f'],{windowsHide:true,stdio:'ignore'});else child.kill('SIGTERM')}}catch{};process.exit(0)}
const server=http.createServer((req,res)=>{
 if(req.url.startsWith('/_fixture/mode')){mode=new URL(req.url,'http://fixture').searchParams.get('value')??'raw';res.end(mode);return}
 if(req.url==='/_fixture/shutdown'){res.end('stopped');setTimeout(shutdown,30);return}
 if(req.url.startsWith('/_codegate/dsh-mobile/')){const name=req.url.split('/').at(-1);if(!['v1.js','v1.css'].includes(name)){res.writeHead(404);res.end();return}res.setHeader('Content-Type',name.endsWith('.js')?'text/javascript':'text/css');res.end(fs.readFileSync(path.join(workspace,'internal/server/dshmobile',name)));return}
 const proxy=http.request(upstreamOptions(req),up=>{
  const headers={...up.headers};delete headers['set-cookie'];
  if(req.url==='/'&&mode!=='raw'){
   const chunks=[];up.on('data',c=>chunks.push(c));up.on('end',()=>{let html=Buffer.concat(chunks).toString();html=html.replace('</head>','<link rel="stylesheet" href="/_codegate/dsh-mobile/v1.css"></head>').replace('</body>','<script id="cg-dsh-mobile-script" src="/_codegate/dsh-mobile/v1.js" data-supported="'+(mode==='adapted'?'true':'false')+'" defer></script></body>');delete headers['content-length'];delete headers.etag;res.writeHead(up.statusCode,headers);res.end(html)});
  }else{res.writeHead(up.statusCode,headers);up.pipe(res)}
 });proxy.on('error',()=>{res.writeHead(502);res.end('isolated proxy unavailable')});req.pipe(proxy);
});
server.on('upgrade',(req,socket,head)=>{
 const proxy=http.request(upstreamOptions(req));proxy.on('upgrade',(res,remote,remoteHead)=>{socket.write('HTTP/1.1 101 Switching Protocols\r\n'+Object.entries(res.headers).map(([k,v])=>`${k}: ${v}`).join('\r\n')+'\r\n\r\n');if(remoteHead.length)socket.write(remoteHead);if(head.length)remote.write(head);remote.pipe(socket);socket.pipe(remote);remote.on('error',()=>socket.destroy());socket.on('error',()=>remote.destroy())});proxy.on('error',()=>socket.destroy());proxy.end();
});
server.listen(port,'127.0.0.1',()=>console.log('isolated real DSH fixture ready on '+port+'; model calls 0; child '+child.pid));
