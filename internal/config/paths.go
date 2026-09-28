package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// 平台默认路径（§12.2 paths.go）。
//
// 两套目录刻意分开：
//   - ConfigDir  存**配置与私钥**（Agent 用）—— 应该随备份走，但权限严格
//   - DataDir    存**运行时数据**（Server 的 SQLite）—— 大、可重建、可迁移
//
// 混在一起会导致「备份配置时把几百 MB 的数据库一起打包」这类问题。
const appDirName = "CodeGate"

// ConfigDir 返回配置与密钥目录。
//
//	Windows: %APPDATA%\CodeGate
//	macOS:   ~/Library/Application Support/CodeGate
//	Linux:   $XDG_CONFIG_HOME/codegate（默认 ~/.config/codegate）
func ConfigDir() string {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, appDirName)
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", appDirName)
		}
	default:
		if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
			return filepath.Join(v, "codegate")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".config", "codegate")
		}
	}
	// 兜底：拿不到任何平台变量时退到当前目录下的相对路径，
	// 而不是 panic —— 让 doctor 有机会把问题报出来。
	return "." + string(filepath.Separator) + "codegate"
}

// DataDir 返回 Server 的运行时数据目录。
//
//	Windows: %LOCALAPPDATA%\CodeGate
//	macOS:   ~/Library/Application Support/CodeGate
//	Linux:   $XDG_DATA_HOME/codegate（默认 ~/.local/share/codegate）
//
// 注意 Windows 上用 LOCALAPPDATA 而不是 APPDATA：漫游配置文件里不该放大文件。
func DataDir() string {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, appDirName)
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", appDirName)
		}
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, "codegate")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "codegate")
		}
	}
	return "." + string(filepath.Separator) + "codegate-data"
}

// DefaultDBPath 返回 Server 默认的 SQLite 文件路径。
func DefaultDBPath() string {
	return filepath.Join(DataDir(), "codegate.db")
}

// DefaultAgentConfigPath 返回 Agent 默认的配置文件路径。
func DefaultAgentConfigPath() string {
	return filepath.Join(ConfigDir(), "agent.json")
}

// DefaultDeviceKeyPath 返回 Agent 设备私钥的默认路径（§10.2）。
//
// ★ 私钥与配置放同一目录但**不放进 agent.json**：
// 配置文件常被复制/粘贴/贴进 issue，私钥绝不能有这种机会。
func DefaultDeviceKeyPath() string {
	return filepath.Join(ConfigDir(), "device.key")
}

// EnsureDir 创建目录（含父目录），权限 0700。
//
// 用 0700 而不是 0755：这个目录里可能有设备私钥与数据库。
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}
