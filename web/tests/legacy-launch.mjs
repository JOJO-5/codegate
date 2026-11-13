import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import {parse} from '@vue/compiler-sfc'
import ts from 'typescript'
import {ref,computed} from 'vue'
const script=parse(await readFile(new URL('../src/views/DeviceDetailView.vue',import.meta.url),'utf8')).descriptor.scriptSetup.content
const ast=ts.createSourceFile('device.ts',script,ts.ScriptTarget.Latest,true)
const declaration=ast.statements.find(n=>ts.isFunctionDeclaration(n)&&n.name?.text==='createSession')
const code=ts.transpileModule(declaration.getText(ast),{compilerOptions:{target:ts.ScriptTarget.ES2022}}).outputText
const saved=new Map(),creating=ref(false),createError=ref(''),drafts={owner:'owner',storageUnavailable:false,get:k=>saved.get(k)||'',set:(k,v)=>v?saved.set(k,v):saved.delete(k)}
const legacyPending={get value(){return !!drafts.get('legacy-pending')}}
let creates=0
const env={creating,createError,legacyPending,legacyPendingKey:computed(()=> 'legacy-pending'),drafts,props:{id:'device'},conn:{isOpen:true},cwd:ref('/demo'),selectedPreset:ref({id:'codex'}),launchMode:ref(''),nativeCommand:ref({native_launch_modes:[]}),launchModeIssue:()=>'',worktree:ref(true),sessionName:ref('legacy task'),estimateTerminalSize:()=>({cols:80,rows:24}),humanizeError:e=>e.message,sessions:{create:async()=>{creates++;throw Object.assign(Error('lost'),{code:'request_outcome_unknown'})}},nativeCLI:()=>'',localStorage:{setItem:()=>{}},cwdKey:ref('cwd'),pendingFile:ref(''),router:{push:async()=>{}},auth:{user:{id:'owner'}}}
const create=new Function(...Object.keys(env),`let launchGeneration=0;${code};return createSession`)(...Object.values(env))
await create()
assert.equal(saved.size,1,'legacy launch also saves a durable intention before dispatch')
await create()
assert.equal(creates,1,'legacy entry cannot bypass unknown-outcome protection')
assert.equal(creating.value,false)
console.log('PASS legacy device launch uses durable identity and shared result-recovery entry')
