package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type workflowReceipt struct {
	Owner          string                     `json:"owner"`
	Hash           string                     `json:"hash"`
	Workflow       protocol.Workflow          `json:"workflow"`
	MasterNativeID string                     `json:"master_native_id,omitempty"`
	Profiles       []protocol.WorkflowProfile `json:"profiles"`
}
type workflowRun struct {
	receipt workflowReceipt
	cancel  context.CancelFunc
}

func workflowHash(w *protocol.Workflow) string {
	b, _ := json.Marshal(struct {
		ID, Owner, Device, Goal, Cwd, Master string
		Free                                 bool
		Turns, Minutes                       int
	}{w.ID, w.UserID, w.DeviceID, w.Goal, w.Cwd, w.MasterID, w.FreeWorkers, w.MaxTurns, w.MaxMinutes})
	return promptDigest(string(b))
}
func (a *Agent) workflowReceiptPath(id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", errors.New("无效调度身份")
	}
	return filepath.Join(a.cfg.StateDir, "workflows", id+".json"), nil
}
func (a *Agent) saveWorkflowReceipt(r workflowReceipt) error {
	path, err := a.workflowReceiptPath(r.Workflow.ID)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeLiveJSON(path, r)
}
func (a *Agent) readWorkflowReceipt(id, user string) (workflowReceipt, error) {
	var r workflowReceipt
	path, err := a.workflowReceiptPath(id)
	if err != nil {
		return r, err
	}
	file, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer file.Close()
	err = json.NewDecoder(file).Decode(&r)
	if err != nil {
		return r, err
	}
	if r.Owner != user {
		return r, errors.New("调度记录不属于当前账号")
	}
	r.Workflow.UserID = user
	return r, nil
}
func cloneWorkflow(w protocol.Workflow) protocol.Workflow {
	b, _ := json.Marshal(w)
	var copy protocol.Workflow
	_ = json.Unmarshal(b, &copy)
	copy.UserID = w.UserID
	return copy
}

func (a *Agent) onWorkflowControl(req *protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.WorkflowControl](req)
	if err != nil || p.UserID == "" {
		a.replyError(req, protocol.NewError(protocol.CodeInvalidPayload, "调度请求身份无效"))
		return
	}
	if p.Action == "catalog" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		done, err := a.beginActivity()
		if err != nil {
			a.replyError(req, err)
			return
		}
		defer done()
		profiles, err := a.workflowProfiles(ctx)
		if err != nil {
			a.replyError(req, protocol.NewError(protocol.CodeInvalidPayload, err.Error()))
			return
		}
		a.reply(req, protocol.TypeWorkflowState, protocol.WorkflowReply{Profiles: profiles})
		return
	}
	if _, err = uuid.Parse(p.ID); err != nil {
		a.replyError(req, protocol.NewError(protocol.CodeInvalidPayload, "无效调度身份"))
		return
	}
	if p.Workflow != nil {
		p.Workflow.UserID = p.UserID
	}
	a.tasksMu.Lock()
	if run := a.workflowRuns[p.ID]; run != nil {
		if run.receipt.Owner != p.UserID {
			a.tasksMu.Unlock()
			a.replyError(req, protocol.NewError(protocol.CodeForbidden, "调度记录不可访问"))
			return
		}
		if p.Action == "start" && (p.Workflow == nil || workflowHash(p.Workflow) != run.receipt.Hash) {
			a.tasksMu.Unlock()
			a.replyError(req, protocol.NewError(protocol.CodeInvalidPayload, "同一调度标识内容不一致"))
			return
		}
		if p.Action == "cancel" && !protocol.WorkflowTerminal(run.receipt.Workflow.State) {
			run.cancel()
		}
		copy := cloneWorkflow(run.receipt.Workflow)
		a.tasksMu.Unlock()
		a.reply(req, protocol.TypeWorkflowState, protocol.WorkflowReply{Workflow: &copy})
		return
	}
	stored, readErr := a.readWorkflowReceipt(p.ID, p.UserID)
	if readErr == nil {
		if p.Action == "start" && (p.Workflow == nil || workflowHash(p.Workflow) != stored.Hash) {
			a.tasksMu.Unlock()
			a.replyError(req, protocol.NewError(protocol.CodeInvalidPayload, "同一调度标识内容不一致"))
			return
		}
		if !protocol.WorkflowTerminal(stored.Workflow.State) {
			stored.Workflow.State = "needs_review"
			stored.Workflow.Detail = "Agent 重启后执行结果需要核对；不会重放主 Agent 或工作任务"
			stored.Workflow.Revision++
			stored.Workflow.UpdatedAt = time.Now().UnixMilli()
			_ = a.saveWorkflowReceipt(stored)
		}
		a.tasksMu.Unlock()
		a.reply(req, protocol.TypeWorkflowState, protocol.WorkflowReply{Workflow: &stored.Workflow})
		return
	}
	if !os.IsNotExist(readErr) {
		a.tasksMu.Unlock()
		a.replyError(req, protocol.NewError(protocol.CodeInvalidPayload, "无法安全读取调度回执"))
		return
	}
	if p.Action != "start" {
		row := protocol.Workflow{ID: p.ID, Revision: 1, State: "needs_review", Detail: "未找到可核对的执行回执；不会重新派发"}
		if p.Workflow != nil {
			row.Revision = p.Workflow.AgentRevision + 1
		}

		a.tasksMu.Unlock()
		a.reply(req, protocol.TypeWorkflowState, protocol.WorkflowReply{Workflow: &row})
		return
	}
	w := p.Workflow
	if w == nil || w.ID != p.ID || w.DeviceID != a.deviceUUID().String() || w.Goal == "" || len(w.Goal) > 8000 || w.MaxTurns < 2 || w.MaxTurns > 32 || w.MaxMinutes < 1 || w.MaxMinutes > 180 {
		a.tasksMu.Unlock()
		a.replyError(req, protocol.NewError(protocol.CodeInvalidPayload, "调度目标、设备或运行范围无效"))
		return
	}
	w.UserID = p.UserID
	if !a.tasksClosing && len(a.workflowRuns) > 0 {
		row := protocol.Workflow{ID: p.ID, State: "waiting_idle", Detail: "本机正在执行另一个主 Agent 调度，等待其结束", Revision: w.AgentRevision + 1}
		a.tasksMu.Unlock()
		a.reply(req, protocol.TypeWorkflowState, protocol.WorkflowReply{Workflow: &row})
		return
	}
	if a.tasksClosing {
		a.tasksMu.Unlock()
		a.replyError(req, protocol.NewError(protocol.CodeInvalidPayload, "本机已有调度或正在退出，请等待原任务结束"))
		return
	}
	finish, err := a.beginActivity()
	if err != nil {
		a.tasksMu.Unlock()
		a.replyError(req, err)
		return
	}
	a.quotaMu.Lock()
	parent := a.quotaCtx
	a.quotaMu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(w.MaxMinutes)*time.Minute)
	row := cloneWorkflow(*w)
	row.State = "running"
	row.Detail = "正在准备主 Agent 的独立工作区"
	row.Revision = 1
	row.Steps = []protocol.WorkflowStep{}
	run := &workflowRun{receipt: workflowReceipt{Owner: p.UserID, Hash: workflowHash(w), Workflow: row}, cancel: cancel}
	if err = a.saveWorkflowReceipt(run.receipt); err != nil {
		a.tasksMu.Unlock()
		cancel()
		finish()
		a.replyError(req, err)
		return
	}
	if a.workflowRuns == nil {
		a.workflowRuns = map[string]*workflowRun{}
	}
	a.workflowRuns[w.ID] = run
	a.tasksWG.Add(1)
	copy := cloneWorkflow(row)
	a.tasksMu.Unlock()
	a.reply(req, protocol.TypeWorkflowState, protocol.WorkflowReply{Workflow: &copy})
	go a.runWorkflow(ctx, run, finish)
}

func (a *Agent) runWorkflow(ctx context.Context, run *workflowRun, finish func()) {
	defer a.tasksWG.Done()
	defer finish()
	defer run.cancel()
	defer func() { a.tasksMu.Lock(); delete(a.workflowRuns, run.receipt.Workflow.ID); a.tasksMu.Unlock() }()
	receipt := run.receipt
	state := &receipt.Workflow
	save := func() error {
		state.Revision++
		state.UpdatedAt = time.Now().UnixMilli()
		if err := a.saveWorkflowReceipt(receipt); err != nil {
			return err
		}
		a.tasksMu.Lock()
		run.receipt = receipt
		run.receipt.Workflow = cloneWorkflow(*state)
		a.tasksMu.Unlock()
		env, _ := protocol.NewEnvelope(protocol.TypeWorkflowState, protocol.WorkflowReply{Workflow: state})
		_ = a.sendControl(env)
		return nil
	}
	fail := func(err error) {
		state.State = "needs_review"
		state.Detail = err.Error()
		if errors.Is(err, context.Canceled) {
			state.State = "cancelled"
			state.Detail = "已确认本次调度执行停止；工作区及产物保留"
		} else if errors.Is(err, context.DeadlineExceeded) {
			state.Detail = "达到运行时间上限，已停止本次调度；请核对原生对话与产物"
		}
		_ = save()
	}
	cwd, err := a.ws.Resolve(state.Cwd)
	if err != nil {
		fail(errors.New("项目目录已不在本机授权范围"))
		return
	}
	profiles, err := a.workflowProfiles(ctx)
	if err != nil {
		fail(err)
		return
	}
	var master *protocol.WorkflowProfile
	for _, p := range profiles {
		if p.ID == state.MasterID {
			copy := p
			master = &copy
			break
		}
	}
	if master == nil {
		fail(errors.New("所选主 Agent 配置已不可用，请重新选择"))
		return
	}
	state.MasterLabel = workflowClip(master.Label, 200)
	workers := []protocol.WorkflowProfile{}
	for _, p := range profiles {
		if !state.FreeWorkers || p.Free {
			workers = append(workers, p)
		}
	}
	if len(workers) == 0 {
		fail(errors.New("当前费用策略下没有可用工作模型，请检查免费模型或本机授权"))
		return
	}
	a.workspaceMu.Lock()
	worktree, _, err := createWorktree(ctx, a.ws, cwd)
	if err != nil {
		a.workspaceMu.Unlock()
		fail(errors.New("无法创建独立 Git 工作区，请确认项目有初始提交且目录可写"))
		return
	}
	state.Worktree = worktree
	state.BaseCommit, _, _ = runGit(ctx, worktree, "rev-parse", "HEAD")
	state.Branch, _, _ = runGit(ctx, worktree, "branch", "--show-current")
	state.BaseCommit = strings.TrimSpace(state.BaseCommit)
	state.Branch = strings.TrimSpace(state.Branch)
	receipt.Profiles = profiles
	err = save()
	a.workspaceMu.Unlock()
	if err != nil {
		fail(errors.New("工作区已创建，但调度回执无法保存；未调用模型"))
		return
	}
	driver := workflowDriver{state: state, profiles: workers, save: save, artifact: func(ctx context.Context) (workflowArtifact, error) {
		return workflowFiles(ctx, worktree, state.BaseCommit)
	}, check: func(ctx context.Context, id string) (string, error) { return workflowCheck(ctx, worktree, id) }}
	driver.ask = func(ctx context.Context, prompt string) (string, error) {
		result, err := a.workflowTurn(ctx, *master, worktree, receipt.MasterNativeID, prompt, true, func(id string) error { receipt.MasterNativeID = id; state.MasterNativeID = id; return save() })
		if result.ID != "" {
			receipt.MasterNativeID = result.ID
		}
		if err != nil {
			return "", err
		}
		if result.State != "completed" {
			return "", errors.New("主 Agent 执行未成功：" + workflowClip(result.Text, 300))
		}
		return result.Text, nil
	}
	driver.worker = func(ctx context.Context, p protocol.WorkflowProfile, instruction string) (workflowTurnResult, error) {
		result, err := a.workflowTurn(ctx, p, worktree, "", "这是 CodeGate 主 Agent 分配的工作任务。只在当前独立工作区实现；保留原始主目录，不提交、不推送、不部署。新增/修改界面和功能必须考虑可发现性、概念模型、及时真实反馈、防错、取消、恢复、键盘和窄屏。不要更改本机 CLI 凭据或配置。OpenCode 工作模型只使用原生文件工具，不运行 shell、不启动子 Agent；实际测试由主 Agent 的 check 操作执行。如果指令要求运行命令，请报告由主 Agent 验证，不要重试被拒绝的工具。实现完立即用最终文本报告修改和未验证项。\n任务："+instruction, false, func(id string) error { state.Steps[len(state.Steps)-1].NativeID = id; return save() })
		return result, err
	}
	if err = driver.drive(ctx); err != nil {
		fail(err)
	}
}
