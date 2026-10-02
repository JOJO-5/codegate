package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jojo/codegate/internal/agent"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/updatefile"
)

type installedAgent struct {
	Path            string `json:"path"`
	Version         string `json:"version,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	Size            int64  `json:"size,omitempty"`
	Previous        string `json:"previous,omitempty"`
	PreviousVersion string `json:"previous_version,omitempty"`
	PreviousSHA256  string `json:"previous_sha256,omitempty"`
	PreviousSize    int64  `json:"previous_size,omitempty"`
}
type pendingAgent struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	SHA256  string `json:"sha256,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

func updatePath(stateDir, name string) string { return filepath.Join(stateDir, "updates", name) }

func queueVerifiedUpdate(a *agent.Agent, stateDir, path, version string, stop context.CancelFunc) error {
	if !validUpdatePath(stateDir, path, version) {
		return errors.New("更新包路径或版本无效")
	}
	digest, size, err := artifactDigest(stateDir, path, version)
	if err != nil {
		return err
	}
	// Stage the durable ticket before freezing: a full disk must not stop a
	// working Agent merely because it could not allocate a pending-state file.
	tmp, err := os.CreateTemp(updatePath(stateDir, ""), ".pending-*")
	if err != nil {
		return err
	}
	pathTmp := tmp.Name()
	tmp.Close()
	defer os.Remove(pathTmp)
	if err = updatefile.WriteJSON(pathTmp, pendingAgent{Path: path, Version: version, SHA256: digest, Size: size}); err != nil {
		return err
	}
	if !a.FreezeForUpdate() {
		return agent.ErrUpdateBusy
	}
	defer stop()
	return updatefile.Replace(pathTmp, updatePath(stateDir, "pending.json"))

}
func validUpdatePath(stateDir, path, version string) bool {
	if !strings.HasPrefix(version, "v") || !agentVersion(version) {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	root, err := filepath.Abs(updatePath(stateDir, ""))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	name := "codegate-agent-" + version + "-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return rel == name
}
func agentVersion(v string) bool {
	if !strings.HasPrefix(v, "v") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return false
			}
		}
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return false
		}
	}
	return true
}
func versionAfter(candidate, current string) bool {
	if !agentVersion(candidate) || !agentVersion(current) {
		return false
	}
	next := strings.Split(candidate[1:], ".")
	old := strings.Split(current[1:], ".")
	for i := range next {
		a, _ := strconv.ParseUint(next[i], 10, 32)
		b, _ := strconv.ParseUint(old[i], 10, 32)
		if a != b {
			return a > b
		}
	}
	return false
}
func artifactDigest(stateDir, path, version string) (string, int64, error) {
	if !validUpdatePath(stateDir, path, version) {
		return "", 0, errors.New("不可信更新路径")
	}
	if err := updatefile.EnsureDir(updatePath(stateDir, "")); err != nil {
		return "", 0, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 100<<20 {
		return "", 0, errors.New("更新包不是普通文件或大小无效")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(f, (100<<20)+1))
	if err != nil || n != info.Size() {
		return "", 0, errors.New("更新包读取期间发生变化")
	}
	return hex.EncodeToString(sum.Sum(nil)), n, nil
}
func verifyArtifact(stateDir string, req pendingAgent) error {
	digest, size, err := artifactDigest(stateDir, req.Path, req.Version)
	if err != nil {
		return err
	}
	if req.Size != size || len(req.SHA256) != 64 || !strings.EqualFold(req.SHA256, digest) {
		return errors.New("更新包与已校验票据不一致")
	}
	return nil
}
func consumePending(stateDir string) (pendingAgent, error) {
	path := updatePath(stateDir, "pending.json")
	var req pendingAgent
	err := updatefile.ReadJSON(path, &req)
	if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(path)
	}
	if err != nil {
		return pendingAgent{}, err
	}
	if err = verifyArtifact(stateDir, req); err != nil {
		return pendingAgent{}, err
	}
	return req, nil
}

func cmdSupervise(args []string) error {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath, "", "")
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, err = filepath.Abs(self)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return supervise(ctx, *configPath, cfg.StateDir, self, agent.Version, defaultSupervisorPolicy())
}

type supervisorPolicy struct {
	StartupTimeout  time.Duration
	ProgressTimeout time.Duration
	StableWindow    time.Duration
	Poll            time.Duration
	RestartDelay    time.Duration
}

func defaultSupervisorPolicy() supervisorPolicy {
	return supervisorPolicy{90 * time.Second, 3 * time.Minute, 10 * time.Minute, time.Second, 5 * time.Second}
}

func supervise(ctx context.Context, configPath, stateDir, self, selfVersion string, p supervisorPolicy) error {
	if err := updatefile.EnsureDir(updatePath(stateDir, "")); err != nil {
		return err
	}
	current := installedAgent{Path: self, Version: selfVersion}
	var saved installedAgent
	if updatefile.ReadJSON(updatePath(stateDir, "current.json"), &saved) == nil && saved.Version != "" && (saved.Version == selfVersion || versionAfter(saved.Version, selfVersion)) {
		if validInstalled(stateDir, saved, self, selfVersion) {
			current = saved
			if retryBlocked(stateDir, saved.Version) && saved.Previous != "" {
				current = previousAgent(saved)
			}
		} else {
			fmt.Fprintln(os.Stderr, "忽略校验失败的 Agent 更新状态，使用本机启动器")
		}
	}
	var running *managedChild
	var marker string
	failures := 0
	for ctx.Err() == nil {
		if running == nil {
			var err error
			if current.Path != self {
				err = verifyArtifact(stateDir, pendingAgent{current.Path, current.Version, current.SHA256, current.Size})
			}
			if err == nil {
				running, marker, err = launchManaged(ctx, current.Path, configPath, stateDir)
			}
			if err != nil {
				if current.Previous == "" {
					return err
				}
				_ = writeUpdateFailure(stateDir, current.Version, "当前版本无法启动或校验失败，已回滚")
				current = previousAgent(current)
				if err = writeState(updatePath(stateDir, "current.json"), current); err != nil {
					return err
				}
				failures = 0
				continue
			}
		}
		started := time.Now()
		reason := monitorManaged(ctx, running, marker, current.Version, p)
		running.stop()
		_ = os.Remove(marker)
		_ = os.Remove(marker + ".legacy")
		running = nil
		if ctx.Err() != nil {
			return nil
		}
		req, err := consumePending(stateDir)
		if err == nil && versionAfter(req.Version, current.Version) {
			if retryBlocked(stateDir, req.Version) {
				fmt.Fprintln(os.Stderr, "失败版本仍在退避期，继续使用旧版本")
			} else {
				err = checkCandidate(ctx, req)
				var next *managedChild
				var nextMarker string
				if err == nil {
					next, nextMarker, err = launchManaged(ctx, req.Path, configPath, stateDir)
				}
				if err == nil && awaitReadyWithPolicy(ctx, next, nextMarker, req.Version, p) {
					nextState := installedAgent{Path: req.Path, Version: req.Version, SHA256: req.SHA256, Size: req.Size, Previous: current.Path, PreviousVersion: current.Version, PreviousSHA256: current.SHA256, PreviousSize: current.Size}
					if err = writeState(updatePath(stateDir, "current.json"), nextState); err == nil {
						current = nextState
						running = next
						marker = nextMarker
						failures = 0
						continue
					}
				}
				if next != nil {
					next.stop()
					_ = os.Remove(nextMarker)
					_ = os.Remove(nextMarker + ".legacy")
				}
				if ctx.Err() != nil {
					return nil
				}
				_ = writeUpdateFailure(stateDir, req.Version, "新版本未通过产物、启动或 Server 鉴权检查，继续使用旧版")
				fmt.Fprintln(os.Stderr, "Agent 新版本未通过检查，继续使用旧版本:", req.Version)
			}
		} else {
			// Count late crashes too; only a full stable interval resets the budget.
			if time.Since(started) >= p.StableWindow {
				failures = 0
			}
			failures++
			if current.Previous != "" && (reason == "stalled" || failures >= 3) {
				_ = writeUpdateFailure(stateDir, current.Version, "新版本进度停止或连续异常退出，已回滚")
				current = previousAgent(current)
				if err := writeState(updatePath(stateDir, "current.json"), current); err != nil {
					return err
				}
				failures = 0
				fmt.Fprintln(os.Stderr, "Agent 新版本持续健康检查失败，已回滚")
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(p.RestartDelay):
		}
	}
	return nil
}
func previousAgent(current installedAgent) installedAgent {
	return installedAgent{Path: current.Previous, Version: current.PreviousVersion, SHA256: current.PreviousSHA256, Size: current.PreviousSize}
}
func validInstalled(stateDir string, current installedAgent, self, selfVersion string) bool {
	valid := func(path, version, digest string, size int64) bool {
		if path == self {
			return version == selfVersion
		}
		return verifyArtifact(stateDir, pendingAgent{path, version, digest, size}) == nil
	}
	return valid(current.Path, current.Version, current.SHA256, current.Size) && (current.Previous == "" || valid(current.Previous, current.PreviousVersion, current.PreviousSHA256, current.PreviousSize))
}

type managedChild struct {
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
}

func (c *managedChild) kill() {
	c.once.Do(func() {
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})
	<-c.done
}
func (c *managedChild) stop() {
	select {
	case <-c.done:
		return
	default:
	}
	if c.cmd.Process != nil {
		if err := c.cmd.Process.Signal(os.Interrupt); err != nil {
			c.kill()
			return
		}
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-c.done:
		return
	case <-timer.C:
		c.kill()
	}
}
func launchManaged(ctx context.Context, binary, configPath, stateDir string) (*managedChild, string, error) {
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	file, err := os.CreateTemp(updatePath(stateDir, ""), ".health-*")
	if err != nil {
		return nil, "", err
	}
	marker := file.Name()
	file.Close()
	os.Remove(marker)
	cmd := exec.Command(binary, "run", "-config", configPath)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CODEGATE_AGENT_READY_FILE=") && !strings.HasPrefix(e, "CODEGATE_AGENT_HEALTH_FILE=") && !strings.HasPrefix(e, "CODEGATE_AGENT_SUPERVISED=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "CODEGATE_AGENT_SUPERVISED=1", "CODEGATE_AGENT_HEALTH_FILE="+marker, "CODEGATE_AGENT_READY_FILE="+marker+".legacy")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err = cmd.Start(); err != nil {
		return nil, "", err
	}
	child := &managedChild{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(child.done) }()
	return child, marker, nil
}
func readHealth(marker, version string, now time.Time, timeout time.Duration) (protocol.ManagedHealth, bool) {
	var health protocol.ManagedHealth
	if updatefile.ReadJSON(marker, &health) != nil {
		return health, false
	}
	age := now.Sub(time.UnixMilli(health.UpdatedAt))
	return health, health.Version == version && health.UpdatedAt > 0 && age >= -5*time.Second && age < timeout
}
func awaitReady(ctx context.Context, child *managedChild, marker, version string) bool {
	return awaitReadyWithPolicy(ctx, child, marker, version, defaultSupervisorPolicy())
}
func awaitReadyWithPolicy(ctx context.Context, child *managedChild, marker, version string, p supervisorPolicy) bool {
	timer := time.NewTimer(p.StartupTimeout)
	defer timer.Stop()
	tick := time.NewTicker(p.Poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-child.done:
			return false
		case <-timer.C:
			return false
		case now := <-tick.C:
			health, ok := readHealth(marker, version, now, p.ProgressTimeout)
			if ok && health.Authenticated {
				return true
			}
		}
	}
}
func monitorManaged(ctx context.Context, child *managedChild, marker, version string, p supervisorPolicy) string {
	tick := time.NewTicker(p.Poll)
	defer tick.Stop()
	lastProgress := time.Now()
	for {
		select {
		case <-ctx.Done():
			return "cancelled"
		case <-child.done:
			return "exited"
		case now := <-tick.C:
			if health, ok := readHealth(marker, version, now, p.ProgressTimeout); ok {
				lastProgress = time.UnixMilli(health.UpdatedAt)
			}
			if now.Sub(lastProgress) > p.ProgressTimeout {
				return "stalled"
			}
		}
	}
}

type probeOutput struct {
	bytes.Buffer
	truncated bool
}

func (b *probeOutput) Write(data []byte) (int, error) {
	n := len(data)
	if b.Len()+n > 4096 {
		b.truncated = true
		data = data[:4096-b.Len()]
	}
	_, _ = b.Buffer.Write(data)
	return n, nil
}
func checkCandidate(ctx context.Context, req pendingAgent) error {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, req.Path, "version")
	cmd.WaitDelay = time.Second
	output := &probeOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		return err
	}
	if output.truncated || strings.TrimSpace(output.String()) != "codegate-agent "+req.Version {
		return errors.New("更新包版本不匹配")
	}
	return nil
}

func writeState(path string, state installedAgent) error { return updatefile.WriteJSON(path, state) }
func retryBlocked(stateDir, version string) bool {
	var failure protocol.AgentUpdateFailure
	return updatefile.ReadJSON(updatePath(stateDir, "last-failure.json"), &failure) == nil && failure.Version == version && failure.RetryAfter > time.Now().UnixMilli()
}
func writeUpdateFailure(stateDir, version, reason string) error {
	path := updatePath(stateDir, "last-failure.json")
	var old protocol.AgentUpdateFailure
	_ = updatefile.ReadJSON(path, &old)
	attempts := 1
	if old.Version == version {
		attempts = old.Attempts + 1
		if attempts < 1 {
			attempts = 1
		}
	}
	delay := 15 * time.Minute
	for i := 1; i < attempts && delay < 24*time.Hour; i++ {
		delay *= 2
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	now := time.Now()
	return updatefile.WriteJSON(path, protocol.AgentUpdateFailure{Version: version, Reason: reason, OccurredAt: now.UnixMilli(), Attempts: attempts, RetryAfter: now.Add(delay).UnixMilli()})
}
