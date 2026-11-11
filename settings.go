package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ============================================================================
// 本地设置
//
// 不再使用 json 配置文件。所有设置都通过对话界面录入，保存到本地：
//
//   ~/.sshex/settings     模型接口设置（key=value 纯文本）
//   ~/.sshex/servers/     每台服务器一个文件
//   ~/.sshex/MEMORY.md    长期记忆
//   ~/.sshex/known_hosts  主机指纹
//
// settings 是极简的 key=value 格式，方便手改，也不需要 JSON 语法。
// ============================================================================

// Settings 是持久化的模型设置。
type Settings struct {
	Provider string // 展示名，例如 DeepSeek
	BaseURL  string
	APIKey   string
	Model    string
}

// dataHome 返回数据目录。
//
// 默认就是可执行文件所在目录（和 Ai 放在一起），所有文件都在这里：
//
//	<运行目录>/settings          模型接口与 key
//	<运行目录>/servers/          每台服务器
//	<运行目录>/MEMORY.md         记忆
//
// 不在用户主目录里创建任何东西，删掉整个文件夹即可清理干净。
// 仍可用 $SSHEX_HOME 指定其它位置。
func dataHome() string {
	if v := os.Getenv("SSHEX_HOME"); v != "" {
		return expandHome(v)
	}
	if dir := execDir(); dir != "" {
		return dir
	}
	// 极端情况（拿不到可执行文件路径）：退回到当前工作目录
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// execDir 返回可执行文件所在目录（解析符号链接）。
func execDir() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Dir(exe)
}

func settingsPath() string { return filepath.Join(dataHome(), "settings") }

// legacyHome 是旧版本的 ~/.sshex，只用于提示用户旧文件还在。
func legacyHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".sshex")
}

// LoadSettings 读取本地设置；文件不存在返回零值。
func LoadSettings() (*Settings, error) {
	data, err := os.ReadFile(settingsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &Settings{}, nil
		}
		return nil, err
	}
	s := &Settings{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "provider":
			s.Provider = v
		case "base_url":
			s.BaseURL = v
		case "api_key":
			s.APIKey = v
		case "model":
			s.Model = v
		}
	}
	return s, nil
}

// Save 写入本地设置（0600）。
func (s *Settings) Save() error {
	dir := dataHome()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# sshex 设置\n")
	fmt.Fprintf(&b, "provider=%s\n", s.Provider)
	fmt.Fprintf(&b, "base_url=%s\n", s.BaseURL)
	fmt.Fprintf(&b, "api_key=%s\n", s.APIKey)
	fmt.Fprintf(&b, "model=%s\n", s.Model)
	tmp := settingsPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, settingsPath())
}

// Complete 判断设置是否可用。
func (s *Settings) Complete() bool {
	if s == nil || s.BaseURL == "" || s.Model == "" {
		return false
	}
	if isLocalEndpoint(s.BaseURL) {
		return true
	}
	if s.APIKey == "" {
		return false
	}
	return undefinedEnvRef(s.APIKey) == ""
}

// 生成运行时配置（内部使用，不落盘）。
func (s *Settings) runtimeConfig() *Config {
	cfg := &Config{
		Version: 1,
		Name:    "sshex",
		Model: ModelConfig{
			Name:    s.Provider,
			BaseURL: s.BaseURL,
			APIKey:  s.APIKey,
			Model:   s.Model,
		},
		DataDir: dataHome(),
		Policy: Policy{
			MaxParallel:   4,
			ConfirmRisk:   true,
			ContinueOnErr: true,
		},
		Alert: Alert{
			DiskUsedPercent: 85,
			MemUsedPercent:  90,
			SwapUsedPercent: 50,
			LoadPerCore:     2,
		},
	}
	cfg.dir = dataHome()
	cfg.path = settingsPath()
	return cfg
}
