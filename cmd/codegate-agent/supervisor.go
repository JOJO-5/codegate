package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/jojo/codegate/internal/agent"
	"github.com/jojo/codegate/internal/protocol"
)

type installedAgent struct {
	Path string `json:"path"`
	Previous string `json:"previous,omitempty"`
}

type pendingAgent struct {
	Path string `json:"path"`
	Version string `json:"version"`
}

func updatePath(stateDir, name string) string {
	return filepath.Join(stateDir, "updates", name)
}

// The Agent calls this only after a verified download. FreezeIfEmpty is the
// final, atomic barrier against a session being created before the handoff.
func queueVerifiedUpdate(a *agent.Agent, stateDir, path, version string, stop context.CancelFunc) error {
	if !validUpdatePath(stateDir, path, version) {
		return errors.New("更新包路径或版本无效")
	}
	dir := filepath.Join(stateDir, "updates")
	data, err := json.Marshal(pendingAgent{Path: path, Version: version})
	if err != nil { return err }
	tmp, err := os.CreateTemp(dir, ".pending-*")
	if err != nil { return err }
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil { tmp.Close(); return err }
	if err := tmp.Sync(); err != nil { tmp.Close(); return err }
	if err := tmp.Close(); err != nil { return err }
	if !a.Manager().FreezeIfEmpty() { return nil }
	defer stop()
	target := updatePath(stateDir, "pending.json")
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) { return err }
	return os.Rename(tmp.Name(), target)
}

func validUpdatePath(stateDir, path, version string) bool {
	if !strings.HasPrefix(version, "v") || !agentVersion(version) { return false }
	abs, err := filepath.Abs(path)
	if err != nil { return false }
	root, err := filepath.Abs(filepath.Join(stateDir, "updates"))
	if err != nil { return false }
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) { return false }
	name := "codegate-agent-" + version + "-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" { name += ".exe" }
	return rel == name
}

func agentVersion(v string) bool {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 { return false }
	for _, p := range parts {
		if p == "" { return false }
		for _, c := range p { if c < '0' || c > '9' { return false } }
	}
	return true
}

func cmdSupervise(args []string) error {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil { return err }
	cfg, err := loadConfig(*configPath, "", "")
	if err != nil { return err }
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "updates"), 0o700); err != nil { return err }
	self, err := os.Executable()
	if err != nil { return err }
	self, err = filepath.Abs(self)
	if err != nil { return err }
	stateFile := updatePath(cfg.StateDir, "current.json")
	current := installedAgent{Path: self}
	failures := 0
	if data, err := os.ReadFile(stateFile); err == nil {
		var saved installedAgent
		if json.Unmarshal(data, &saved) == nil && saved.Path != "" {
			if _, err := os.Stat(saved.Path); err == nil { current = saved }
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	for ctx.Err() == nil {
		started := time.Now()
		wait, oldMarker, err := launchManaged(ctx, current.Path, *configPath, cfg.StateDir)
		if err != nil {
			if current.Previous == "" { return err }
			fmt.Fprintln(os.Stderr, "Agent 当前版本无法启动，回滚旧版本:", err)
			_ = writeUpdateFailure(cfg.StateDir, current.Path, "当前版本无法启动: "+err.Error())
			current = installedAgent{Path: current.Previous}
			if err := writeState(stateFile, current); err != nil { return err }
			failures = 0
			continue
		}
		select {
		case <-ctx.Done():
			wait.kill()
			return nil
		case <-wait.done:
		}
		_ = os.Remove(oldMarker)
		if ctx.Err() != nil { return nil }
		if time.Since(started) < 30*time.Second { failures++ } else { failures = 0 }
		if failures >= 3 && current.Previous != "" {
			_ = writeUpdateFailure(cfg.StateDir, current.Path, "新版本连续异常退出，已回滚")
			current = installedAgent{Path: current.Previous}
			_ = writeState(stateFile, current)
			failures = 0
			fmt.Fprintln(os.Stderr, "Agent 连续异常退出，已回滚旧版本")
		}
		candidate, err := consumePending(cfg.StateDir)
		if err == nil && candidate.Path != "" && candidate.Path != current.Path {
			if err := checkCandidate(ctx, candidate); err == nil {
				next, readyFile, err := launchManaged(ctx, candidate.Path, *configPath, cfg.StateDir)
				if err == nil && awaitReady(ctx, next, readyFile, candidate.Version) {
					nextState := installedAgent{Path: candidate.Path, Previous: current.Path}
					if err := writeState(stateFile, nextState); err == nil {
						current = nextState
						_ = os.Remove(updatePath(cfg.StateDir, "last-failure.json"))
						failures = 0
						select {
						case <-ctx.Done(): next.kill(); return nil
						case <-next.done:
						}
						if ctx.Err() != nil { return nil }
						failures = 1
						continue
					}
					next.kill()
				}
			}
			_ = writeUpdateFailure(cfg.StateDir, candidate.Version, "新版本未通过启动和 Server 鉴权健康检查，继续使用旧版")
			fmt.Fprintln(os.Stderr, "Agent 新版本启动失败，继续使用旧版本:", candidate.Version)
		}
		// An existing version can exit on a transient failure. Restart with
		// backoff; a verified candidate is only accepted after Server auth.
		select { case <-ctx.Done(): return nil; case <-time.After(5*time.Second): }
	}
	return nil
}

type managedChild struct {
	cmd *exec.Cmd
	done chan error
}

func (c *managedChild) kill() {
	if c.cmd.Process != nil { _ = c.cmd.Process.Kill() }
	<-c.done
}

func launchManaged(ctx context.Context, binary, configPath, stateDir string) (*managedChild, string, error) {
	ready, err := os.CreateTemp(filepath.Join(stateDir, "updates"), ".ready-*")
	if err != nil { return nil, "", err }
	readyFile := ready.Name()
	ready.Close()
	os.Remove(readyFile)
	cmd := exec.Command(binary, "run", "-config", configPath)
	cmd.Env = append(os.Environ(), "CODEGATE_AGENT_SUPERVISED=1", "CODEGATE_AGENT_READY_FILE="+readyFile)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil { return nil, "", err }
	child := &managedChild{cmd:cmd, done:make(chan error, 1)}
	go func(){ child.done <- cmd.Wait() }()
	return child, readyFile, nil
}

func awaitReady(ctx context.Context, child *managedChild, marker, version string) bool {
	timer := time.NewTimer(90*time.Second)
	defer timer.Stop()
	tick := time.NewTicker(250*time.Millisecond)
	defer tick.Stop()
	defer os.Remove(marker)
	for {
		select {
		case <-ctx.Done():
			child.kill()
			return false
		case <-child.done:
			return false
		case <-timer.C:
			child.kill()
			return false
		case <-tick.C:
			data, err := os.ReadFile(marker)
			if err == nil && string(data) == version { return true }
		}
	}
}

func consumePending(stateDir string) (pendingAgent, error) {
	path := updatePath(stateDir, "pending.json")
	data, err := os.ReadFile(path)
	if err != nil { return pendingAgent{}, err }
	_ = os.Remove(path)
	var req pendingAgent
	if err := json.Unmarshal(data, &req); err != nil { return pendingAgent{}, err }
	if !validUpdatePath(stateDir, req.Path, req.Version) { return pendingAgent{}, errors.New("不可信的更新请求") }
	return req, nil
}

func checkCandidate(ctx context.Context, req pendingAgent) error {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(c, req.Path, "version").CombinedOutput()
	if err != nil { return err }
	if strings.TrimSpace(string(output)) != "codegate-agent "+req.Version {
		return fmt.Errorf("更新包版本不匹配: %q", strings.TrimSpace(string(output)))
	}
	return nil
}

func writeState(path string, state installedAgent) error {
	data, err := json.Marshal(state)
	if err != nil { return err }
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".current-*")
	if err != nil { return err }
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil { tmp.Close(); return err }
	if err := tmp.Sync(); err != nil { tmp.Close(); return err }
	if err := tmp.Close(); err != nil { return err }
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) { return err }
	return os.Rename(tmp.Name(), path)
}

func writeUpdateFailure(stateDir, version, reason string) error {
	failure := protocol.AgentUpdateFailure{Version: version, Reason: reason, OccurredAt: time.Now().UnixMilli()}
	data, err := json.Marshal(failure)
	if err != nil { return err }
	path := updatePath(stateDir, "last-failure.json")
	tmp, err := os.CreateTemp(filepath.Dir(path), ".last-failure-*")
	if err != nil { return err }
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil { tmp.Close(); return err }
	if err := tmp.Close(); err != nil { return err }
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) { return err }
	return os.Rename(tmp.Name(), path)
}
