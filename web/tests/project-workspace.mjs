import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'
const source=await readFile(new URL('../src/lib/projectWork.ts',import.meta.url),'utf8')
const code=ts.transpileModule(source,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
const {inProject,taskGroup,workItems}=await import(`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`)
assert.equal(inProject('D:\\work\\Demo\\','d:/work/demo','windows'),true)
assert.equal(inProject('/work/Demo','/work/demo','linux'),false)
assert.equal(inProject('/work/demo-child','/work/demo','linux'),false,'siblings and subdirectories are not implicitly part of a saved project')
assert.equal(taskGroup('continuation','queued','time'),'planned')
assert.equal(taskGroup('continuation','waiting_wall','live_quota'),'active')
assert.equal(taskGroup('workflow','ready_for_review'),'review','delivery is not user acceptance')
assert.equal(taskGroup('workflow','unknown_future_state'),'review','unknown states need inspection')
assert.equal(taskGroup('continuation','failed'),'review','failures must remain discoverable')
assert.equal(taskGroup('continuation','cancelled'),'history')
const rows=workItems('device',[{id:'same',state:'running',source:{name:'Continue',cwd:'/demo'},updated_at:1},{id:'deleted',state:'completed',deleted_at:1,source:{cwd:'/demo'}}],[{id:'same',goal:'Master',cwd:'/demo',state:'ready_for_review',updated_at:2}])
assert.equal(rows.length,2)
assert.notEqual(rows[0].key,rows[1].key,'source kinds with identical IDs remain distinct')
assert.ok(rows.some(r=>r.kind==='workflow'&&r.group==='review'))
console.log('PASS project boundaries, honest task groups and source identities')
