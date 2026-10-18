package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jojo/codegate/internal/protocol"
	"net/url"
	"strings"
	"time"
)

type workflowModel struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Provider     string `json:"providerID"`
	Status       string `json:"status"`
	Capabilities struct {
		Tools bool `json:"tools"`
	} `json:"capabilities"`
	Cost []struct {
		Input  *float64 `json:"input"`
		Output *float64 `json:"output"`
		Cache  struct {
			Read  *float64 `json:"read"`
			Write *float64 `json:"write"`
		} `json:"cache"`
	} `json:"cost"`
}

func (m workflowModel) free() bool {
	if len(m.Cost) == 0 {
		return false
	}
	for _, tier := range m.Cost {
		for _, price := range []*float64{tier.Input, tier.Output, tier.Cache.Read, tier.Cache.Write} {
			if price == nil || *price != 0 {
				return false
			}
		}
	}
	return true
}
func workflowModels(ctx context.Context, c *openCodeV2Client) ([]workflowModel, error) {
	var data struct {
		Data []workflowModel `json:"data"`
	}
	path := "/api/model"
	if c.cwd != "" {
		path += "?location[directory]=" + url.QueryEscape(c.cwd)
	}
	err := c.call(ctx, "GET", path, nil, &data)
	return data.Data, err
}
func workflowProfileID(c ResolvedCommand, p protocol.WorkflowProfile) string {
	p.ID = ""
	b, _ := json.Marshal(struct {
		C ResolvedCommand
		P protocol.WorkflowProfile
	}{c, p})
	return "profile-" + promptDigest(string(b))[:24]
}
func (a *Agent) workflowProfiles(ctx context.Context) ([]protocol.WorkflowProfile, error) {
	cfg := a.commandConfig()
	env := BuildEnv(cfg, 80, 24)
	result := []protocol.WorkflowProfile{}
	add := func(c ResolvedCommand, p protocol.WorkflowProfile) {
		p.ID = workflowProfileID(c, p)
		for _, old := range result {
			if old.ID == p.ID {
				return
			}
		}
		result = append(result, p)
	}
	for _, spec := range cfg.AllowedCommands {
		c, err := cfg.ResolveCommand(spec.ID, "", nil, false)
		if err != nil {
			continue
		}
		switch nativeTool(c) {
		case "codex":
			add(c, protocol.WorkflowProfile{Label: spec.Label + " · 本机默认模型", CommandID: spec.ID, Tool: "codex"})
			control, err := openCodexControl(ctx, codexHome(env))
			if err != nil {
				continue
			}
			var models struct {
				Data []struct {
					Model       string `json:"model"`
					DisplayName string `json:"displayName"`
					Hidden      bool   `json:"hidden"`
				} `json:"data"`
			}
			err = control.call("model/list", map[string]any{"includeHidden": false}, &models)
			control.close()
			if err == nil {
				for _, m := range models.Data {
					if m.Model != "" && !m.Hidden {
						add(c, protocol.WorkflowProfile{Label: spec.Label + " · " + m.DisplayName, CommandID: spec.ID, Tool: "codex", Model: m.Model})
					}
				}
			}
		case "opencode":
			check, stop := context.WithTimeout(ctx, 12*time.Second)
			v2, err := isOpenCodeV2(check, c, env, "")
			if err != nil || !v2 {
				stop()
				continue
			}
			client, err := connectOpenCodeV2(check, c, env, "")
			if err != nil {
				stop()
				continue
			}
			models, err := workflowModels(check, client)
			stop()
			if err != nil {
				continue
			}
			for _, m := range models {
				if m.Provider == "opencode" && m.Status == "active" && m.Capabilities.Tools && m.free() {
					add(c, protocol.WorkflowProfile{Label: spec.Label + " · " + m.Name + "（免费）", CommandID: spec.ID, Tool: "opencode", Model: m.ID, Provider: m.Provider, Free: true})
				}
			}
		}
	}
	for _, p := range cfg.WorkflowProfiles {
		c, err := cfg.ResolveCommand(p.CommandID, "", nil, false)
		if err != nil {
			continue
		}
		tool := nativeTool(c)
		if tool != "codex" && tool != "opencode" {
			continue
		}
		p.Tool = tool
		if tool == "codex" {
			p.Free = false
		}
		if p.Label == "" {
			p.Label = p.CommandID + " · " + p.Model
		}
		add(c, p)
	}
	if len(result) == 0 {
		return result, errors.New("本机没有可用的 Codex 或 OpenCode V2 调度配置，请先安装并授权 CLI")
	}
	return result, nil
}
func (a *Agent) workflowCommand(p protocol.WorkflowProfile) (ResolvedCommand, error) {
	c, err := a.commandConfig().ResolveCommand(p.CommandID, "", nil, false)
	if err != nil {
		return c, err
	}
	if nativeTool(c) != p.Tool || workflowProfileID(c, p) != p.ID {
		return c, errors.New("CLI 配置已变化，调度已停止；请重新选择主 Agent")
	}
	if strings.ContainsAny(p.Model+p.Provider, "\x00\r\n") {
		return c, errors.New("模型配置无效")
	}
	return c, nil
}
