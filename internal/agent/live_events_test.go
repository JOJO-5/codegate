package agent

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeLifecycleRecordsOnlyIdentityAndErrorCategory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "conversation-markers")
	os.MkdirAll(dir, 0700)
	marker := filepath.Join(dir, uuid.NewString()+".json")
	id := uuid.NewString()
	invoke := func(event, code, prompt string) {
		b, _ := json.Marshal(map[string]any{"session_id": id, "cwd": root, "hook_event_name": event, "error": code, "prompt": prompt, "last_assistant_message": "PRIVATE TRANSCRIPT", "error_details": "SECRET TOKEN"})
		if e := RecordLiveEvent([]string{marker}, strings.NewReader(string(b))); e != nil {
			t.Fatal(e)
		}
	}
	invoke("UserPromptSubmit", "", "task secret text")
	path := marker + ".live." + id + ".json"
	initial, e := readLiveEvent(path)
	if e != nil || initial.State != "inProgress" {
		t.Fatal(initial, e)
	}
	invoke("StopFailure", "rate_limit", "")
	wall, e := readLiveEvent(path)
	if e != nil || wall.TurnID != initial.TurnID || wall.Code != "usageLimitExceeded" || wall.State != "failed" {
		t.Fatal(wall, e)
	}
	b, _ := os.ReadFile(path)
	for _, secret := range []string{"PRIVATE TRANSCRIPT", "SECRET TOKEN", "task secret text"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("hook persisted private text")
		}
	}
	invoke("UserPromptSubmit", "", "continue")
	next, _ := readLiveEvent(path)
	if next.TurnID == wall.TurnID || next.PromptHash != promptDigest("continue") {
		t.Fatal(next)
	}
	invoke("Stop", "", "")
	done, _ := readLiveEvent(path)
	if done.State != "completed" || done.TurnID != next.TurnID {
		t.Fatal(done)
	}
}
func TestNativeOpenCodeAndDSHBridgesPreserveConversationAndPrepareBeforeSubmit(t *testing.T) {
	node, e := exec.LookPath("node")
	if e != nil {
		t.Skip("Node not installed")
	}
	for _, tool := range []string{"opencode", "dsh"} {
		t.Run(tool, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, uuid.NewString()+".json")
			plugin := filepath.Join(root, "plugin.mjs")
			code := openCodeBindingPlugin
			if tool == "dsh" {
				code = dshBindingPlugin
			}
			if e := os.WriteFile(plugin, []byte(code), 0600); e != nil {
				t.Fatal(e)
			}
			script := `import {writeFile,readFile} from 'node:fs/promises';import assert from 'node:assert/strict';import crypto from 'node:crypto';import {pathToFileURL} from 'node:url';
const tool=process.argv[2],plugin=await import(pathToFileURL(process.argv[3])),marker=process.env.CODEGATE_CONVERSATION_MARKER;
const id=tool==='opencode'?'ses_fixture':crypto.randomUUID(),cwd=process.cwd();let starts=0,controller,lastPrompt;
const handlers=new Map();const event=async(type,data)=>{for(const h of handlers.get(type)??[])await h({id,header:{cwd,delegationDepth:0}},data)};
if(tool==='dsh'){
 const agent={status:'idle',followup(message){assert.equal(message.role,'user');lastPrompt=message.content[0].text;starts++;agent.status='running';void event('session/event',{type:'turn/start',data:{turn:2}})},cancel(){agent.status='idle'}};
 const ctx={get(name){if(name==='agents')return {get(target){assert.equal(target,id);return agent}}},on(type,fn){handlers.set(type,[...(handlers.get(type)??[]),fn])}};plugin.default(ctx);await event('session/event',{type:'turn/end',data:{turn:1,reason:{kind:'error',error:{code:'RATE_LIMIT',status:429}}}});
}else{
 let hooks;const client={session:{get:async()=>({data:{id,directory:cwd}}),status:async()=>({data:{[id]:{type:'idle'}}}),messages:async()=>({data:[{info:{role:'user',id:'msg_native_generated'},parts:[{type:'text',text:lastPrompt}]}]}),promptAsync:async(request)=>{assert.equal(request.path.id,id);assert.deepEqual(request.body.model,{providerID:'approved-provider',modelID:'approved-model'});lastPrompt=request.body.parts[0].text;starts++;return {data:undefined}},abort:async()=>({})}};
 hooks=await plugin.CodeGateBinding({client,directory:cwd});await hooks.event({event:{type:'message.updated',properties:{info:{sessionID:id,id:'msg_first',role:'user'}}}});await hooks.event({event:{type:'message.updated',properties:{info:{sessionID:id,parentID:'msg_first',role:'assistant',providerID:'approved-provider',modelID:'approved-model',mode:'build'}}}});await hooks.event({event:{type:'session.error',properties:{sessionID:id,error:{name:'APIError',data:{statusCode:429,responseBody:'{"error":{"type":"usage_limit_reached"}}'}}}}});await hooks.event({event:{type:'session.idle',properties:{sessionID:id}}});
}
const s=JSON.parse(await readFile(marker+'.live.'+id+'.json','utf8'));assert.equal(s.state,'failed');const r={id:crypto.randomUUID()+'-1',native_id:id,expected_turn:s.turn_id,action:'turn/start',prompt:'continue $(not-shell) & 中文',deadline:Date.now()+5000};await writeFile(marker+'.request.json',JSON.stringify(r));
for(let n=0;n<100;n++){await new Promise(r=>setTimeout(r,20));let ack;try{ack=JSON.parse(await readFile(marker+'.ack.json','utf8'))}catch{}if(ack?.state==='ok')break;if(n===99)throw Error('ack timeout')}
assert.equal(starts,1);assert.equal(lastPrompt,r.prompt);await new Promise(r=>setTimeout(r,250));assert.equal(starts,1);const ack=JSON.parse(await readFile(marker+'.ack.json','utf8'));assert.equal(ack.id,r.id);assert.equal(ack.state,'ok');console.log('PASS '+tool+' original identity, selected model, literal prompt and duplicate submission guard');
`
			path := filepath.Join(root, "test.mjs")
			os.WriteFile(path, []byte(script), 0600)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, node, path, tool, plugin)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CODEGATE_CONVERSATION_MARKER="+marker)
			out, e := cmd.CombinedOutput()
			if e != nil {
				t.Fatalf("%s: %v\n%s", tool, e, out)
			}
			t.Log(string(out))
		})
	}
}

func TestExportLivePluginForNativeSmoke(t *testing.T) {
	path := os.Getenv("CODEGATE_LIVE_PLUGIN_EXPORT")
	if path == "" {
		t.Skip("native CLI smoke export not requested")
	}
	code := openCodeBindingPlugin
	if os.Getenv("CODEGATE_LIVE_PLUGIN_TOOL") == "dsh" {
		code = dshBindingPlugin
	}
	if e := os.WriteFile(path, []byte(code), 0600); e != nil {
		t.Fatal(e)
	}
}
