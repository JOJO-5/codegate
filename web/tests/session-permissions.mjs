import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'
const source=await readFile(new URL('../src/lib/nativeLaunchModes.ts',import.meta.url),'utf8')
const code=ts.transpileModule(source,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
const {launchModeIssue}=await import(`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`)
const modes=[{id:'codex.read-only'},{id:'codex.full-access'}]
assert.equal(launchModeIssue(undefined,''),'','old Agent inherits unchanged')
assert.equal(launchModeIssue(modes,'codex.full-access'),'','explicit native full-access is selectable')
assert.ok(launchModeIssue(modes,'claude.full-access'),'modes cannot cross native tools')
assert.ok(launchModeIssue([], 'codex.read-only'),'unsupported modes do not silently become inherit')
console.log('PASS native CLI launch modes: explicit selection, inheritance, unsupported mode and tool boundaries')
