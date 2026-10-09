package agent

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jojo/codegate/internal/processutil"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

var nativeIDPattern = regexp.MustCompile(`^(ses_[A-Za-z0-9]+|[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12})$`)

func nativeTool(cmd ResolvedCommand) string {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(cmd.Command)), ".exe")
	if name == "cmd" && len(cmd.Args) >= 2 && strings.EqualFold(cmd.Args[0], "/c") {
		name = strings.TrimSuffix(strings.ToLower(filepath.Base(cmd.Args[1])), ".exe")
	}
	switch name {
	case "codex", "opencode", "dsh", "claude":
		return name
	}
	return ""
}

func nativeEnvValue(env []string, name string) string {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
func nativeHome(env []string) string {
	if p := nativeEnvValue(env, "USERPROFILE"); p != "" {
		return p
	}
	return nativeEnvValue(env, "HOME")
}
func codexHome(env []string) string {
	if p := nativeEnvValue(env, "CODEX_HOME"); p != "" {
		return p
	}
	return filepath.Join(nativeHome(env), ".codex")
}
func dshRoot(env []string) string {
	if p := nativeEnvValue(env, "DSH_TUI_SESSION_ROOT"); p != "" {
		return p
	}
	if p := nativeEnvValue(env, "DSH_HOME"); p != "" {
		return filepath.Join(p, "sessions")
	}
	return filepath.Join(nativeHome(env), ".dsh", "sessions")
}
func (a *Agent) privateDSHRoot(key string) (string, error) {
	if _, err := uuid.Parse(key); err != nil {
		return "", errors.New("无效的对话存储标识")
	}
	return filepath.Join(a.cfg.StateDir, "conversations", "dsh", key), nil
}

// Recovery never accepts arbitrary commands or cwd from the browser. For legacy
// records, resolve only a byte-for-byte match with a locally approved command.
func (a *Agent) recoveryCommand(source protocol.SessionSummary) (string, ResolvedCommand, error) {
	cfg := a.commandConfig()
	if source.Recovery != nil && source.Recovery.CommandID != "" {
		cmd, err := cfg.ResolveCommand(source.Recovery.CommandID, "", nil, false)
		if err != nil {
			return "", cmd, err
		}
		if nativeTool(cmd) == "" {
			return "", cmd, errors.New("该命令不支持原生对话恢复")
		}
		return source.Recovery.CommandID, cmd, nil
	}
	for _, spec := range cfg.AllowedCommands {
		if spec.Command == source.Command && reflect.DeepEqual(append([]string{}, spec.Args...), append([]string{}, source.Args...)) {
			cmd, err := cfg.ResolveCommand(spec.ID, "", nil, false)
			if err == nil && nativeTool(cmd) != "" {
				return spec.ID, cmd, nil
			}
		}
	}
	return "", ResolvedCommand{}, errors.New("原命令未获本机授权或配置已变更，无法安全恢复")
}

func (a *Agent) onConversationRequest(env *protocol.Envelope) {
	finish, err := a.beginActivity()
	if err != nil {
		a.replyError(env, protocol.NewError(protocol.CodeInvalidPayload, err.Error()))
		return
	}
	defer finish()
	req, err := protocol.DecodePayload[protocol.ConversationRequest](env)
	if err != nil {
		a.replyError(env, protocol.NewError(protocol.CodeInvalidPayload, err.Error()))
		return
	}
	// Serialize restore with create/removal, including duplicate clicks.
	a.workspaceMu.Lock()
	defer a.workspaceMu.Unlock()
	if live, ok := a.mgr.Get(parseUUID(req.SessionID)); ok && isRecoverableLive(live.Summary()) {
		if env.Type == protocol.TypeConversationRestore {
			a.reply(env, protocol.TypeSessionCreated, protocol.SessionCreatedPayload{Session: live.Summary()})
			return
		}
	}
	id, cmd, err := a.recoveryCommand(req.Source)
	if err != nil {
		a.replyError(env, protocol.NewError(protocol.CodeInvalidPayload, err.Error()))
		return
	}
	cwd, err := a.ws.Resolve(req.Source.Cwd)
	if err != nil {
		a.replyError(env, protocol.NewError(protocol.CodeInvalidPayload, err.Error()))
		return
	}
	childEnv := BuildEnv(a.cfg, req.Cols, req.Rows)
	binding := protocol.ConversationBinding{CommandID: id, SourceID: req.SessionID}
	if req.Source.Recovery != nil {
		binding.NativeID = req.Source.Recovery.NativeID
		binding.StoreKey = req.Source.Recovery.StoreKey
		binding.NativeRoot = req.Source.Recovery.NativeRoot
	}
	if binding.NativeID != "" && req.NativeID != "" && binding.NativeID != req.NativeID {
		a.replyError(env, protocol.NewError(protocol.CodeInvalidPayload, "该记录已经绑定原对话，不能替换成其他对话"))
		return
	}
	target := binding.NativeID
	if target == "" {
		target = req.NativeID
	}
	if binding.NativeRoot != "" {
		childEnv = replaceEnv(childEnv, "DSH_TUI_SESSION_ROOT", binding.NativeRoot)
	}
	history, err := a.nativeHistory(nativeTool(cmd), cwd, childEnv, binding.StoreKey, target)
	if err != nil {
		a.replyError(env, protocol.NewError(protocol.CodeInvalidPayload, err.Error()))
		return
	}
	if env.Type == protocol.TypeConversationList {
		a.reply(env, protocol.TypeConversationListed, protocol.ConversationListed{Conversations: history})
		return
	}
	found := false
	for _, item := range history {
		if item.ID == target {
			found = true
			break
		}
	}
	if !found || !nativeIDPattern.MatchString(target) {
		a.replyError(env, protocol.NewError(protocol.CodeInvalidPayload, "没有找到原生对话：请选择历史记录；记录被删除后无法恢复"))
		return
	}
	binding.NativeID = target
	for _, sum := range a.mgr.Snapshot() {
		if isRecoverableLive(sum) && sum.Cwd == cwd && sum.Recovery != nil && sum.Recovery.CommandID == id && sum.Recovery.NativeID == target && sum.Recovery.StoreKey == binding.StoreKey && sum.Recovery.NativeRoot == binding.NativeRoot {
			copyBinding := *sum.Recovery
			copyBinding.SourceID = req.SessionID
			sum.Recovery = &copyBinding
			a.reply(env, protocol.TypeSessionCreated, protocol.SessionCreatedPayload{Session: sum})
			return
		}
	}
	// createSession normally holds workspaceMu; the locked variant avoids deadlock.
	sess, err := a.createSessionLocked(protocol.SessionCreatePayload{CommandID: id, Cwd: cwd, Name: req.Source.Name, Cols: req.Cols, Rows: req.Rows, Recovery: &binding})
	if err != nil {
		a.replyError(env, protocol.NewError(protocol.CodeInvalidPayload, err.Error()))
		return
	}
	a.reply(env, protocol.TypeSessionCreated, protocol.SessionCreatedPayload{Session: sess.Summary()})
	go a.watchSessionExit(sess)
}
func parseUUID(s string) uuid.UUID { id, _ := uuid.Parse(s); return id }
func isRecoverableLive(s protocol.SessionSummary) bool {
	return s.Status == "running" || s.Status == "starting" || s.Status == "detached"
}

func (a *Agent) nativeHistory(tool, cwd string, env []string, key, target string) ([]protocol.NativeConversation, error) {
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, err
	}
	cwd = resolvedCwd
	if target != "" && !nativeIDPattern.MatchString(target) {
		return nil, errors.New("无效的原生对话 ID")
	}
	if tool == "opencode" {
		return openCodeHistory(cwd, env, target)
	}
	var root string
	switch tool {
	case "codex":
		root = filepath.Join(codexHome(env), "sessions")
	case "claude":
		root = filepath.Join(nativeHome(env), ".claude", "projects")
	case "dsh":
		root = dshRoot(env)
		if key != "" {
			var err error
			root, err = a.privateDSHRoot(key)
			if err != nil {
				return nil, err
			}
		}
	default:
		return nil, errors.New("该 CLI 不支持恢复")
	}
	var out []protocol.NativeConversation
	if tool == "codex" {
		var err error
		out, err = codexDatabaseHistory(cwd, env, target)
		if err != nil {
			return nil, err
		}
	}
	count := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		count++
		if count > 20000 {
			return errors.New("原生历史记录过多，请清理历史后重试")
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") && !strings.HasSuffix(path, ".jsonl.zstd") {
			return nil
		}
		item, recordCwd := readNativeHeader(path, tool)
		if item.ID == "" || !nativeIDPattern.MatchString(item.ID) {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(recordCwd)
		if err != nil {
			return nil
		}
		if !sameDirectory(cwd, resolved) {
			return nil
		}
		if target != "" && item.ID != target {
			return nil
		}
		for _, existing := range out {
			if existing.ID == item.ID {
				return nil
			}
		}
		out = append(out, item)
		return nil
	})
	if os.IsNotExist(err) {
		err = nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	if len(out) > 200 {
		out = out[:200]
	}
	return out, err
}
func sameDirectory(a, b string) bool {
	if os.PathSeparator == '\\' {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func openCodeHistory(cwd string, env []string, target string) ([]protocol.NativeConversation, error) {
	root := nativeEnvValue(env, "XDG_DATA_HOME")
	if root == "" {
		root = filepath.Join(nativeHome(env), ".local", "share")
	}
	path := filepath.Join(root, "opencode", "opencode.db")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []protocol.NativeConversation{}, nil
	}
	// The native DB is always read-only. No migration or locking writes.
	db, err := sql.Open("sqlite", nativeSQLiteURI(path))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	query := `SELECT id,title,directory,time_created,time_updated FROM session WHERE parent_id IS NULL AND time_archived IS NULL`
	args := []any{}
	if target != "" {
		query += " AND id = ?"
		args = append(args, target)
	}
	query += " ORDER BY time_updated DESC LIMIT 10000"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("无法读取 OpenCode 原生历史，请确认 CLI 版本兼容: %w", err)
	}
	defer rows.Close()
	out := []protocol.NativeConversation{}
	for rows.Next() {
		var item protocol.NativeConversation
		var directory string
		if err := rows.Scan(&item.ID, &item.Title, &directory, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		resolved, e := filepath.EvalSymlinks(directory)
		if e == nil && sameDirectory(cwd, resolved) && nativeIDPattern.MatchString(item.ID) {
			item.Title = shortTitle(item.Title)
			out = append(out, item)
		}
		if len(out) >= 200 {
			break
		}
	}
	return out, rows.Err()
}

func readNativeHeader(path, tool string) (protocol.NativeConversation, string) {
	f, err := os.Open(path)
	if err != nil {
		return protocol.NativeConversation{}, ""
	}
	defer f.Close()
	var reader io.Reader = f
	if strings.HasSuffix(path, ".zstd") {
		z, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(8<<20))
		if err != nil {
			return protocol.NativeConversation{}, ""
		}
		defer z.Close()
		reader = z
	}
	scan := bufio.NewScanner(io.LimitReader(reader, 128<<10))
	scan.Buffer(make([]byte, 4096), 64<<10)
	item := protocol.NativeConversation{}
	cwd := ""
	for n := 0; n < 40 && scan.Scan(); n++ {
		var record struct {
			Type            string          `json:"type"`
			SessionID       string          `json:"sessionId"`
			Cwd             string          `json:"cwd"`
			ID              string          `json:"id"`
			CreatedAt       int64           `json:"createdAt"`
			DelegationDepth int             `json:"delegationDepth"`
			Timestamp       string          `json:"timestamp"`
			Payload         json.RawMessage `json:"payload"`
			Message         struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scan.Bytes(), &record) != nil {
			continue
		}
		if tool == "codex" && record.Type == "session_meta" {
			var p struct {
				ID        string `json:"id"`
				Cwd       string `json:"cwd"`
				Timestamp string `json:"timestamp"`
			}
			if json.Unmarshal(record.Payload, &p) == nil {
				item.ID = p.ID
				cwd = p.Cwd
				t, _ := time.Parse(time.RFC3339Nano, p.Timestamp)
				item.CreatedAt = t.UnixMilli()
			}
		}
		if tool == "dsh" && record.Type == "session" {
			if record.DelegationDepth > 0 {
				return protocol.NativeConversation{}, ""
			}
			item.ID = record.ID
			cwd = record.Cwd
			item.CreatedAt = record.CreatedAt
			break
		}
		if tool == "claude" && record.SessionID != "" && record.Cwd != "" {
			item.ID = record.SessionID
			cwd = record.Cwd
			t, _ := time.Parse(time.RFC3339Nano, record.Timestamp)
			item.CreatedAt = t.UnixMilli()
			break
		}
		if tool == "codex" && record.Type == "event_msg" {
			var p struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if json.Unmarshal(record.Payload, &p) == nil && p.Type == "user_message" {
				item.Title = shortTitle(p.Message)
				break
			}
		}
	}
	if st, e := f.Stat(); e == nil {
		item.UpdatedAt = st.ModTime().UnixMilli()
	}
	if item.Title == "" {
		item.Title = item.ID
	}
	return item, cwd
}
func shortTitle(s string) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > 120 {
		r = r[:120]
	}
	return string(r)
}

// prepareConversation adds native integrations without changing login or
// preference directories. An uncertain association remains unbound: explicit
// history selection is safer than guessing the most recent file.
func (a *Agent) prepareConversation(cmd *ResolvedCommand, env []string, cwd, commandID string, binding *protocol.ConversationBinding) ([]string, *protocol.ConversationBinding, string, error) {
	tool := nativeTool(*cmd)
	if tool == "" || commandID == "" {
		return env, nil, "", nil
	}
	if binding == nil {
		binding = &protocol.ConversationBinding{CommandID: commandID}
	} else {
		copy := *binding
		binding = &copy
	}
	if binding.NativeID != "" && !nativeIDPattern.MatchString(binding.NativeID) {
		return nil, nil, "", errors.New("无效的原生对话 ID")
	}
	if binding.NativeID != "" {
		switch tool {
		case "codex":
			cmd.Args = append(cmd.Args, "resume", binding.NativeID)
		case "opencode":
			cmd.Args = append(cmd.Args, "--session", binding.NativeID)
		case "dsh", "claude":
			cmd.Args = append(cmd.Args, "--resume", binding.NativeID)
		}
	}
	markerDir := filepath.Join(a.cfg.StateDir, "conversation-markers")
	if err := os.MkdirAll(markerDir, 0700); err != nil {
		return nil, nil, "", err
	}
	marker := filepath.Join(markerDir, uuid.NewString()+".json")
	switch tool {
	case "dsh":
		if binding.StoreKey != "" {
			root, err := a.privateDSHRoot(binding.StoreKey)
			if err != nil {
				return nil, nil, "", err
			}
			env = replaceEnv(env, "DSH_TUI_SESSION_ROOT", root)
		} else if binding.NativeRoot != "" {
			env = replaceEnv(env, "DSH_TUI_SESSION_ROOT", binding.NativeRoot)
		}
		plugin := filepath.Join(markerDir, "dsh-binding.mjs")
		if err := writePrivatePlugin(plugin, []byte(dshBindingPlugin)); err != nil {
			return nil, nil, "", err
		}
		patch := marker + ".yml"
		name, _ := json.Marshal(plugin)
		if err := os.WriteFile(patch, []byte("- insert:\n    - id: codegate-conversation-binding\n      name: "+string(name)+"\n"), 0600); err != nil {
			return nil, nil, "", err
		}
		prefix := 0
		if strings.EqualFold(filepath.Base(cmd.Command), "cmd.exe") && len(cmd.Args) >= 2 {
			prefix = 2
		}
		args := append([]string{}, cmd.Args[:prefix]...)
		args = append(args, "--patch", patch)
		cmd.Args = append(args, cmd.Args[prefix:]...)
		env = replaceEnv(env, "CODEGATE_CONVERSATION_MARKER", marker)
	case "claude":
		if binding.NativeID == "" {
			binding.NativeID = uuid.NewString()
			cmd.Args = append(cmd.Args, "--session-id", binding.NativeID)
		}
	case "codex":
		// npm's Windows cmd shim strips embedded JSON quotes; preserve startup
		// there and require explicit native history selection on first recovery.
		if strings.EqualFold(filepath.Base(cmd.Command), "cmd.exe") {
			break
		}
		exe, err := os.Executable()
		if err != nil {
			return nil, nil, "", err
		}
		previous, err := codexNotify(codexHome(env), cmd.Args)
		if err != nil {
			return nil, nil, "", err
		}
		old, _ := json.Marshal(previous)
		notify, _ := json.Marshal([]string{exe, "record-conversation", marker, string(old)})
		if binding.NativeID != "" {
			// Keep overrides before the native subcommand: clap's resume-specific
			// -c vector otherwise replaces existing provider/config overrides.
			args := append([]string{}, cmd.Args[:len(cmd.Args)-2]...)
			args = append(args, "-c", "notify="+string(notify), "resume", binding.NativeID)
			cmd.Args = args
		} else {
			cmd.Args = append(cmd.Args, "-c", "notify="+string(notify))
		}
	case "opencode":
		config := map[string]any{}
		if raw := nativeEnvValue(env, "OPENCODE_CONFIG_CONTENT"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &config); err != nil {
				return nil, nil, "", errors.New("OPENCODE_CONFIG_CONTENT 不是有效 JSON")
			}
		}
		plugin := filepath.Join(markerDir, "opencode-binding.mjs")
		if err := writePrivatePlugin(plugin, []byte(openCodeBindingPlugin)); err != nil {
			return nil, nil, "", err
		}
		plugins, _ := config["plugin"].([]any)
		pluginPath := filepath.ToSlash(plugin)
		if filepath.VolumeName(plugin) != "" {
			pluginPath = "/" + pluginPath
		}
		pluginURL := (&url.URL{Scheme: "file", Path: pluginPath}).String()
		config["plugin"] = append(plugins, pluginURL)
		raw, _ := json.Marshal(config)
		env = replaceEnv(env, "OPENCODE_CONFIG_CONTENT", string(raw))
		env = replaceEnv(env, "CODEGATE_CONVERSATION_MARKER", marker)
	}
	return env, binding, marker, nil
}

func codexNotify(home string, args []string) ([]string, error) {
	var config struct {
		Notify []string `toml:"notify"`
	}
	path := filepath.Join(home, "config.toml")
	if _, err := os.Stat(path); err == nil {
		if _, err = toml.DecodeFile(path, &config); err != nil {
			return nil, errors.New("无法解析 Codex 配置，未修改原通知设置")
		}
	}
	for i := 0; i < len(args); i++ {
		v := ""
		if (args[i] == "-c" || args[i] == "--config") && i+1 < len(args) {
			i++
			v = args[i]
		} else if strings.HasPrefix(args[i], "--config=") {
			v = strings.TrimPrefix(args[i], "--config=")
		}
		if strings.HasPrefix(strings.TrimSpace(v), "notify=") || strings.HasPrefix(strings.TrimSpace(v), "notify =") {
			if _, err := toml.Decode(v, &config); err != nil {
				return nil, err
			}
		}
	}
	return config.Notify, nil
}

// RecordConversation is the native Codex notify callback. Retain only ID and cwd;
// prompts, responses and credentials are never written to the marker.
func RecordConversation(args []string) error {
	if len(args) != 3 {
		return errors.New("invalid conversation callback")
	}
	var event struct {
		ThreadID string `json:"thread-id"`
		TurnID   string `json:"turn-id"`
		Cwd      string `json:"cwd"`
	}
	if json.Unmarshal([]byte(args[2]), &event) != nil || !nativeIDPattern.MatchString(event.ThreadID) {
		return errors.New("invalid native conversation")
	}
	data := map[string]string{"id": event.ThreadID, "cwd": event.Cwd}
	if nativeIDPattern.MatchString(event.TurnID) {
		data["turn_id"] = event.TurnID
	}
	raw, _ := json.Marshal(data)
	temp := args[0] + "." + uuid.NewString() + ".tmp"
	if err := os.WriteFile(temp, raw, 0600); err != nil {
		return err
	}
	if err := os.Rename(temp, args[0]); err != nil {
		_ = os.Remove(temp)
		return err
	}
	var previous []string
	if json.Unmarshal([]byte(args[1]), &previous) == nil && len(previous) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, previous[0], append(previous[1:], args[2])...)
		processutil.Background(cmd)
		return cmd.Run()
	}
	return nil
}

const openCodeBindingPlugin = `import {writeFile,rename} from "node:fs/promises";
export const CodeGateBinding = async ({client,directory}) => {let chain=Promise.resolve();return ({
 event: async ({event}) => {
  let info;
  if (event.type === "session.created") info = event.properties.info;
  else if (event.type === "message.updated" && event.properties.info.role === "user") {
   const result = await client.session.get({path:{id:event.properties.info.sessionID}}); info=result.data;
  }
  if (!info || info.parentID || info.directory !== directory) return;
  const marker = process.env.CODEGATE_CONVERSATION_MARKER;
  if (!marker) return;
  chain=chain.then(async()=>{const temp=marker+".tmp";
  await writeFile(temp,JSON.stringify({id:info.id,cwd:directory}),{mode:0o600}); await rename(temp,marker)}).catch(()=>{});await chain;
 }
})};
`

func (a *Agent) watchConversation(sess *session.Session, marker string) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	defer os.Remove(marker)
	defer os.Remove(marker + ".yml")
	read := func() {
		sum := sess.Summary()
		if sum.Recovery == nil {
			return
		}
		id := ""
		if nativeTool(ResolvedCommand{Command: sum.Command, Args: sum.Args}) == "dsh" && sum.Recovery.StoreKey != "" {
			history, err := a.nativeHistory("dsh", sum.Cwd, BuildEnv(a.cfg, sum.Cols, sum.Rows), sum.Recovery.StoreKey, "")
			if err == nil && len(history) == 1 {
				id = history[0].ID
			} else if err == nil && len(history) > 1 {
				if sess.BindConversation("") {
					a.publishConversationBinding(sess)
				}
				return
			} else {
				return
			}
		} else {
			f, err := os.Open(marker)
			if err != nil {
				return
			}
			raw, err := io.ReadAll(io.LimitReader(f, 4096))
			f.Close()
			if err != nil {
				return
			}
			var data struct {
				ID         string `json:"id"`
				Cwd        string `json:"cwd"`
				NativeRoot string `json:"native_root"`
				TurnID     string `json:"turn_id"`
			}
			if json.Unmarshal(raw, &data) != nil || !sameDirectory(data.Cwd, sum.Cwd) || !nativeIDPattern.MatchString(data.ID) {
				return
			}
			id = data.ID
			if nativeTool(ResolvedCommand{Command: sum.Command, Args: sum.Args}) == "codex" {
				// Never persist a notify ID unless the native store proves its identity.
				history, err := a.nativeHistory("codex", sum.Cwd, BuildEnv(a.cfg, sum.Cols, sum.Rows), "", id)
				if err != nil || len(history) != 1 {
					return
				}
			}
			if data.NativeRoot != "" && filepath.IsAbs(data.NativeRoot) {
				if sess.BindConversationIdentity(id, data.NativeRoot) {
					a.publishConversationBinding(sess)
				}
				return
			}
		}
		if nativeIDPattern.MatchString(id) && sess.BindConversation(id) {
			a.publishConversationBinding(sess)
		}
	}
	for {
		select {
		case <-ticker.C:
			read()
		case <-sess.Done():
			read()
			return
		}
	}
}

func writePrivatePlugin(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return nil
	}
	temp := path + "." + uuid.NewString() + ".tmp"
	if err := os.WriteFile(temp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

func (a *Agent) publishConversationBinding(sess *session.Session) {
	bound, _ := protocol.NewEnvelope(protocol.TypeConversationBound, sess.Summary())
	bound.SessionID = sess.ID.String()
	_ = a.sendControl(bound)
}

const dshBindingPlugin = `import {writeFile,rename} from "node:fs/promises";
export default function(ctx) {
 let chain=Promise.resolve();
 const record=(session)=>{
  if (!session?.header || (session.header.delegationDepth??0)>0 || session.header.cwd !== process.cwd()) return;
  const marker=process.env.CODEGATE_CONVERSATION_MARKER;if (!marker) return;
  const root=ctx.get('sessionPersistence')?.root;
  chain=chain.then(async()=>{const temp=marker+".tmp";await writeFile(temp,JSON.stringify({id:session.id,cwd:session.header.cwd,native_root:root}),{mode:0o600});await rename(temp,marker)}).catch(()=>{});
 };
 ctx.on('session/created',record);
 ctx.on('session/event',(session,event)=>{if(event.type==='user/message'||event.type==='message'||event.type==='user_message'||event.type==='prompt')record(session)});
}
`

// Current Codex can store paginated histories in SQLite without a full JSONL.
// Read only metadata from the highest native schema version, never transcripts.
func codexDatabaseHistory(cwd string, env []string, target string) ([]protocol.NativeConversation, error) {
	paths, _ := filepath.Glob(filepath.Join(codexHome(env), "state_*.sqlite"))
	path := ""
	version := -1
	for _, p := range paths {
		v, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "state_"), ".sqlite"))
		if err == nil && v > version {
			version = v
			path = p
		}
	}
	if path == "" {
		return nil, nil
	}
	uri := nativeSQLiteURI(path)
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := "SELECT id,title,cwd,created_at,updated_at FROM threads WHERE archived=0"
	args := []any{}
	if target != "" {
		query += " AND id=?"
		args = append(args, target)
	}
	query += " ORDER BY updated_at DESC LIMIT 10000"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errors.New("无法读取 Codex 原生历史数据库，请确认 CLI 版本兼容")
	}
	defer rows.Close()
	var out []protocol.NativeConversation
	for rows.Next() {
		var item protocol.NativeConversation
		var dir string
		if err = rows.Scan(&item.ID, &item.Title, &dir, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		resolved, e := filepath.EvalSymlinks(dir)
		if e != nil || !sameDirectory(cwd, resolved) || !nativeIDPattern.MatchString(item.ID) {
			continue
		}
		item.CreatedAt *= 1000
		item.UpdatedAt *= 1000
		if len([]rune(item.Title)) > 120 {
			item.Title = string([]rune(item.Title)[:120])
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func nativeSQLiteURI(path string) string {
	p := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro"}).String()
}
