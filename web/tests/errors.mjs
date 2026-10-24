import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'
const url = s => `data:text/javascript;base64,${Buffer.from(s).toString('base64')}`
const compile = s => ts.transpileModule(s, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText
const apiURL = url(compile(await readFile(new URL('../src/lib/api.ts', import.meta.url), 'utf8')))
const { ApiError } = await import(apiURL)
const wsURL = url('export class WSError extends Error {}')
const source = await readFile(process.argv[2] ?? new URL('../src/lib/format.ts', import.meta.url), 'utf8')
const format = compile(source).replace("from './api'", `from '${apiURL}'`).replace("from './ws'", `from '${wsURL}'`)
const { humanizeError } = await import(url(format))
assert.match(humanizeError(new ApiError(503, 'device_offline', 'offline')), /设备离线/)
assert.match(humanizeError(new ApiError(504, 'agent_timeout', 'timeout')), /Agent 响应超时/)
assert.match(humanizeError(new ApiError(503, 'live_check_unavailable', '无法核对原会话，尚未开启自动续跑；请检查电脑与 Agent 连接后重试')), /尚未开启自动续跑/)
assert.match(humanizeError(new ApiError(409, 'dsh_port_in_use', 'untrusted detail')), /3080.*占用/)
assert.match(humanizeError(new ApiError(503, 'dsh_unhealthy', 'untrusted detail')), /不会自动重启/)
assert.match(humanizeError(new ApiError(504, 'dsh_timeout', 'untrusted detail')), /结果.*未确认/)
assert.match(humanizeError(new ApiError(500, 'internal', 'private detail')), /服务端出错/)
assert.match(humanizeError(new ApiError(401, 'auth', 'expired')), /登录已失效/)
console.log('PASS 离线和超时保留业务原因，未知服务错误及认证错误使用对应提示')
