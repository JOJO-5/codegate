import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'
import { parse, compileScript } from '@vue/compiler-sfc'
import { ref, computed, watch, reactive } from 'vue'

const url = s => `data:text/javascript;base64,${Buffer.from(s).toString('base64')}`
const helperSource = await readFile(new URL('../src/lib/clipboardImages.ts', import.meta.url), 'utf8')
const helpers = await import(url(ts.transpileModule(helperSource, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText))
const png = new File(['image bytes'], 'capture.png', { type: 'image/png' })
const text = new File(['text'], 'note.txt', { type: 'text/plain' })
const data = { files: [png, text], items: [{kind:'file',type:'image/png',getAsFile:()=>png}] }
assert.deepEqual(helpers.pastedImages(data), [png], 'files and items represent the same image once')
assert.deepEqual(helpers.pastedImages({files:[],items:data.items}), [png], 'items-only clipboard works')
assert.deepEqual(helpers.pastedImages(null), [])
let prevented=0, stopped=0, received=[]
const event={clipboardData:data,preventDefault:()=>prevented++,stopImmediatePropagation:()=>stopped++}
assert.equal(helpers.captureImagePaste(event, files=>{received=files}), true)
assert.deepEqual(received,[png]);assert.equal(prevented,1);assert.equal(stopped,1)
assert.equal(helpers.captureImagePaste({...event,clipboardData:{files:[],items:[]}},()=>assert.fail()), false)
assert.equal(prevented,1,'text-only paste retains native behavior')
const clipboardImages=await helpers.readClipboardImages({read:async()=>[{types:['text/html','image/png','image/jpeg'],getType:async type=>new Blob(['image'],{type})}]})
assert.equal(clipboardImages.length,1,'one representation per clipboard item')
assert.match(clipboardImages[0].name,/\.png$/)
await assert.rejects(helpers.readClipboardImages(undefined), /快捷键|选择图片/)
await assert.rejects(helpers.readClipboardImages({read:async()=>{throw new DOMException('denied','NotAllowedError')}}))

let unmounts=[];let uploadCalls=[];let failUpload=false
const conn=reactive({isOpen:true});let read=async()=>[png]
globalThis.__clipboardTest={ref,computed,watch,conn,unmounts,
 readClipboardImages:()=>read(),
 uploadFile:async(...args)=>{uploadCalls.push(args);if(failUpload)throw Error('network failed')},
}
const shim=url(`const t=globalThis.__clipboardTest;export const {ref,computed,watch,readClipboardImages,uploadFile}=t;
export const defineComponent=x=>x,onUnmounted=f=>t.unmounts.push(f),useConnStore=()=>t.conn;
export const humanizeError=e=>e.message,attachmentFilename=name=>'unique-'+name,MAX_UPLOAD_SIZE=104857600;`)
const source=await readFile(new URL('../src/components/SessionAttachments.vue',import.meta.url),'utf8')
let code=ts.transpileModule(compileScript(parse(source).descriptor,{id:'clipboard-test'}).content,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
code=code.replace(/from (['"])[^'"\n]+\1/g,`from '${shim}'`)
const component=(await import(url(code))).default
const setup=()=>component.setup({sessionId:'session',enabled:true,nativeImages:false},{expose:()=>{},emit:()=>{}})
const ctx=setup()
ctx.addFiles([png])
assert.equal(ctx.items.value.length,1)
assert.equal(ctx.items.value[0].state,'selected')
assert.equal(uploadCalls.length,0,'pasting never uploads or sends automatically')
assert.match(ctx.status.value,/1.*上传/)
failUpload=true;await ctx.upload()
assert.equal(ctx.items.value[0].state,'failed')
assert.match(ctx.items.value[0].error,/network failed/)
failUpload=false;await ctx.upload()
assert.equal(ctx.items.value[0].state,'ready','failed pasted image can retry')
assert.equal(uploadCalls[0][1],'session')
const clipboard=setup()
read=async()=>{throw new DOMException('denied','NotAllowedError')}
await clipboard.pasteImages()
assert.match(clipboard.error.value,/快捷键|选择图片/)
read=async()=>[];await clipboard.pasteImages()
assert.match(clipboard.error.value,/没有.*图片/)
read=async()=>[png];await clipboard.pasteImages()
assert.equal(clipboard.items.value.length,1,'permission failure recovers with another paste')
let finishRead
read=()=>new Promise(resolve=>{finishRead=resolve})
const oldRead=clipboard.pasteImages()
assert.equal(clipboard.reading.value,true)
clipboard.close()
finishRead([png]);await oldRead
assert.equal(clipboard.items.value.length,1,'closing drops a late clipboard response')
const disposed=setup();const stale=disposed.pasteImages();unmounts.at(-1)();finishRead([png]);await stale
assert.equal(disposed.items.value.length,0,'session navigation cannot add stale images')
const interrupted=setup();const interruptedRead=interrupted.pasteImages()
conn.isOpen=false;conn.isOpen=true
finishRead([png]);await interruptedRead
assert.equal(interrupted.items.value.length,0,'connection loss invalidates a late clipboard read even after reconnect')
const limits=setup();limits.addFiles(Array(21).fill(png))
assert.equal(limits.items.value.length,20)
assert.match(limits.error.value,/20/)
const large=setup();large.addFiles([{name:'huge.png',size:104857601,type:'image/png'}])
assert.equal(large.items.value.length,0);assert.match(large.error.value,/100 MB/)
const offline=setup();conn.isOpen=false;offline.addFiles([png]);assert.equal(offline.items.value.length,0)
conn.isOpen=true;offline.addFiles([png]);assert.equal(offline.items.value.length,1)
for(const f of unmounts)f()
const parentSource=await readFile(new URL('../src/views/SessionView.vue',import.meta.url),'utf8')
const parent=parse(parentSource).descriptor
assert.match(parent.template.content,/@paste\.capture="onImagePaste"/,'image interception runs before xterm default paste')
const program=ts.createSourceFile('session.ts',parent.scriptSetup.content,ts.ScriptTarget.Latest,true)
const handler=program.statements.find(n=>ts.isFunctionDeclaration(n)&&n.name?.text==='onImagePaste')
let keys
const findKeys=node=>{if(ts.isCallExpression(node)&&ts.isPropertyAccessExpression(node.expression)&&node.expression.name.text==='attachCustomKeyEventHandler')keys=node.arguments[0];ts.forEachChild(node,findKeys)}
findKeys(program)
const keyJs=ts.transpileModule('const handle='+keys.getText(program),{compilerOptions:{target:ts.ScriptTarget.ES2022}}).outputText
const keyHandler=new Function(`${keyJs};return handle`)()
for(const [ctrlKey,metaKey] of [[true,false],[false,true]]) assert.equal(keyHandler({type:'keydown',key:'v',ctrlKey,metaKey,shiftKey:false,altKey:false}),false,'native paste key must reach the browser without a Ctrl+V terminal byte')
const handlerJs=ts.transpileModule(handler.getText(program),{compilerOptions:{target:ts.ScriptTarget.ES2022}}).outputText
globalThis.Element=class {constructor(scope){this.scope=scope} closest(){return this.scope}}
let added=0
for(const [controller,online,scope,expected] of [[true,true,true,1],[false,true,true,0],[true,false,true,0],[true,true,false,0]]) {
 const open=ref(false),notice=ref('')
 const handle=new Function('captureImagePaste','summary','role','isLive','conn','attachmentsOpen','attachments','clipboardStatus',`${handlerJs};return onImagePaste`)(helpers.captureImagePaste,ref({}),ref(controller?'controller':'viewer'),ref(true),{isOpen:online},open,ref({addFiles:files=>{added+=files.length}}),notice)
 const before=added
 handle({...event,target:new Element(scope)})
 assert.equal(added-before,expected)
 assert.equal(open.value,expected===1)
 if(scope&&(!online||!controller))assert.match(notice.value,/重新粘贴/)
}
console.log('PASS clipboard image capture, text passthrough, single representation, preview-only queue, retry, limits, offline recovery and stale read rejection')
