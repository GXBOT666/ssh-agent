package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
	"strings"
)

// ============================================================================
// 配置结构：与 config.json 一一对应
//
// 顶层顺序刻意按"密钥 -> 身份 -> 工作内容"组织，方便阅读。
// ============================================================================

// Config 是整份配置文件的根对象。
type Config struct {
	// 元信息
	Version     int    `json:"version"`     // 配置格式版本，目前为 1
	Name        string `json:"name"`        // 工具/项目名称
	Description string `json:"description"` // 本配置的用途说明

	// 密钥：所有凭据集中在这里，服务器通过名字引用，避免重复写
	Keys []Key `json:"keys"`

	// 身份介绍：默认身份 + 每个服务器的个人身份
	Identities IdentityBook `json:"identities"`

	// 目标服务器
	Servers []Server `json:"servers"`

	// 工作内容：要执行的运维任务
	Tasks []Task `json:"tasks"`

	// 大模型接口（终端 AI 助手使用，兼容 OpenAI 协议的各类模型）
	Model ModelConfig `json:"model"`

	// 数据目录：服务器密码文件（servers/）、known_hosts 等运行时数据
	DataDir string `json:"data_dir"`

	// 运行策略与告警阈值
	Policy Policy `json:"policy"`
	Alert  Alert  `json:"alert"`
	Report Report `json:"report"`

	// 运行时字段（不参与 JSON）
	path     string
	dir      string
	warnings []string
}

// Key 是一条凭据。type 可为 password / private_key / agent / none 之一，
// 或者直接给 value 字段由程序自动识别（推荐，见 Key.resolveType）。
type Key struct {
	Name        string `json:"name"`        // 必填，唯一；server.key / jump.key 引用它
	Type        string `json:"type"`        // password | private_key | agent | none
	Username    string `json:"username"`    // 该密钥绑定的用户名（可选，server 未指定时使用）
	Value       string `json:"value"`       // 密码 / 私钥 PEM 文本 / agent socket 路径，可用 ${ENV} 引用环境变量
	Path        string `json:"path"`        // 私钥文件路径，相对路径相对于配置文件目录，支持 ~
	KnownHosts  string `json:"known_hosts"` // 仅 private_key：对应私钥的 known_hosts（.pub 文件），用于生成主机指纹
	Passphrase  string `json:"passphrase"`  // 私钥口令（仅加密私钥需要）
	Description string `json:"description"` // 人读说明
}

// IdentityBook 是"身份介绍"。
type IdentityBook struct {
	Default Identity   `json:"default"` // 缺省身份：当 server 没有自己的身份时使用
	Servers []Identity `json:"servers"` // 每台服务器的个人身份介绍
}

// Identity 描述一台服务器（或默认身份）的"自我介绍"。
// 这些字段会随每次连接展示给操作者，也被写进巡检报告。
type Identity struct {
	Name        string   `json:"name"`        // 机器名/展示名
	Role        string   `json:"role"`        // 角色：web / db / cache ...
	Owner       string   `json:"owner"`       // 负责人
	Tags        []string `json:"tags"`        // 标签，run/hosts 可据此筛选
	Description string   `json:"description"` // 这台机器是干什么的
	Notes       []string `json:"notes"`       // 补充说明 / 注意事项
}

// Server 是一台目标机。
type Server struct {
	ID       string   `json:"id"`       // 必填，唯一标识，host 筛选可用
	Name     string   `json:"name"`     // 显示名，默认等于 ID
	Desc     string   `json:"desc"`     // 主机的简短描述
	Host     string   `json:"host"`     // 必填，IP 或域名
	Port     int      `json:"port"`     // 默认 22
	Username string   `json:"username"` // 登录用户，默认取 key.username 或 $USER
	Key      string   `json:"key"`      // 引用 keys[].name；为空表示不需要认证（罕见）
	Tags     []string `json:"tags"`     // 主机标签
	Role     string   `json:"role"`     // 角色（身份里没写时使用）
	Group    string   `json:"group"`    // 分组，run -g 可筛选
	Owner    string   `json:"owner"`
	Notes    []string `json:"notes"`

	HostKey *HostKey `json:"host_key"`      // 主机指纹策略
	Jump    *Jump    `json:"jump"`          // 通过跳板机连接
	Proxy   string   `json:"proxy_command"` // 自定义 ProxyCommand 字面量命令（与 jump 二选一）
	// proxy_tokens 形式的 ProxyCommand：程序按数组直接 exec，不经过 shell，
	// 支持占位符 %h %p %r %k（本地私钥路径）。
	ProxyTokens []string `json:"proxy_tokens"`

	TimeoutSec int   `json:"timeout_sec"` // 连接超时，默认 15s
	Enabled    *bool `json:"enabled"`     // 默认 true；显式 false 则跳过
}

// HostKey 是主机公钥校验策略。
type HostKey struct {
	Policy string `json:"policy"` // strict（默认，需已知指纹）| accept-new（首次记录）| insecure（跳过）
	// 已信任的主机公钥，推荐写 SHA256 指纹；
	// 也可以直接写 authorized_keys 格式的公钥（ssh-ed25519 AAAA... 注释）。
	Fingerprints []string `json:"fingerprints"`
	KnownHosts   string   `json:"known_hosts"` // known_hosts 文件，默认 ~/.ssh/known_hosts
	// 是否强制校验主机密钥（默认 true）。仅当目标设备无法完成密钥交换校验时
	// 才考虑设为 false；关闭后指纹只用于显示。
	Verify *bool `json:"verify"`
}

// Jump 是跳板机设置。
type Jump struct {
	Server   string `json:"server"`   // 引用另一台 server 的 id
	Host     string `json:"host"`     // 或者直接写地址
	Port     int    `json:"port"`     // 默认 22
	Username string `json:"username"` // 留空时取 key 的用户名或当前登录用户
	Key      string `json:"key"`      // 引用 keys[].name
}

// Task 是一段"工作内容"。
type Task struct {
	Name        string `json:"name"`        // 必填，唯一
	Type        string `json:"type"`        // command | upload | download
	Description string `json:"description"` // 这项工作的目的
	Command     string `json:"command"`     // type=command，可用 {{.Var}} 模板
	// type=command 时的检查方式
	Expect     string `json:"expect"`      // 期望在输出中出现的文本（可选）
	ExitCode   *int   `json:"exit_code"`   // 期望退出码，默认 0
	OutputFile string `json:"output_file"` // 输出保存到本地文件（相对配置文件目录），支持 {{.Host}}
	// type=upload / download
	Local     string `json:"local"`      // 本地路径，upload=源文件，download=目标文件名
	Remote    string `json:"remote"`     // 远端路径
	Mode      string `json:"mode"`       // upload 时远端文件权限，默认 0644
	Overwrite *bool  `json:"overwrite"`  // download 时是否覆盖本地已有文件，默认 false
	ExecAfter string `json:"exec_after"` // upload 后追加执行的命令（可选）

	// type=command 时，若配置了 inspect，则按类型解析输出并与 alert 阈值比较。
	Inspect *Inspect `json:"inspect"`

	Hosts      []string          `json:"hosts"`       // 主机的 id/name，为空=全部
	Groups     []string          `json:"groups"`      // 按分组筛选
	Tags       []string          `json:"tags"`        // 任务标签
	Role       string            `json:"role"`        // 只对某角色执行
	Vars       map[string]string `json:"vars"`        // 命令模板变量
	TimeoutSec int               `json:"timeout_sec"` // 默认 60s
	Enabled    *bool             `json:"enabled"`     // 默认 true

	echo string // 运行时：verbose 时打印的命令
}

// Inspect 声明命令输出的解析方式，结果会与 Alert 阈值比较并写入报告。
type Inspect struct {
	Kind string `json:"kind"` // df（按挂载点解析磁盘）/ meminfo（解析 内存/Swap）/ load（解析负载）
	// df：只关心这些挂载点的使用率；为空表示所有本地挂载点
	Mounts []string `json:"mounts"`
}

// Policy 是运行策略。
type Policy struct {
	MaxParallel   int      `json:"max_parallel"`      // 并发主机数，默认 4
	FailFast      bool     `json:"fail_fast"`         // 某台主机失败后停止后续任务
	ContinueOnErr bool     `json:"continue_on_error"` // 默认 true，单个任务失败不影响其他任务
	ConfirmRisk   bool     `json:"confirm_risk"`      // 默认 true，执行含风险关键字的命令前确认
	DenyPatterns  []string `json:"deny_patterns"`     // 即使确认也禁止的命令（正则），为空则用内置
	AllowPatterns []string `json:"allow_patterns"`    // 命中即视为允许，优先级最高
	Privileged    []string `json:"privileged_users"`  // 这些用户执行任务前打印警告
	AuditLog      string   `json:"audit_log"`         // 审计日志文件，相对配置文件目录
}

// Alert 是巡检告警阈值。
type Alert struct {
	DiskUsedPercent float64 `json:"disk_used_percent"` // 磁盘使用率超过它告警，默认 85
	MemUsedPercent  float64 `json:"mem_used_percent"`  // 内存使用率超过它告警，默认 90
	SwapUsedPercent float64 `json:"swap_used_percent"` // swap 使用率超过它告警，默认 50
	LoadPerCore     float64 `json:"load_per_core"`     // 1 分钟负载/核数超过它告警，默认 2
}

// Report 是报告默认值。
type Report struct {
	Dir  string `json:"dir"`           // 默认报告目录，相对配置文件目录
	Host bool   `json:"include_hosts"` // 报告里附服务器身份清单
}

// ============================================================================
// 加载
// ============================================================================

// 查找默认配置文件。
func defaultConfigPaths() []string {
	paths := []string{"config.json"}
	if home, err := os.UserHomeDir(); err == nil {
		// 主目录下的配置优先级次之：无论从哪个目录启动，都能找到同一份服务器和记忆
		paths = append(paths,
			filepath.Join(home, "config.json"),
			filepath.Join(home, ".config", "sshex", "config.json"),
		)
	}
	return paths
}

func findConfig(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("配置文件不存在: %s: %w", explicit, err)
		}
		return explicit, nil
	}
	for _, p := range defaultConfigPaths() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("找不到配置文件，请用 -c 指定，例如: sshex -c ./config.json check")
}

// LoadConfig 读取、解析、补默认值并校验配置。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	// 兼容 UTF-8 BOM
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	cfg := &Config{}
	dec := json.NewDecoder(bytes.NewReader(data))
	// 关闭未知字段校验不严的问题：拼错字段时要报错，而不是静默忽略。
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败 %s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg.path = abs
	cfg.dir = filepath.Dir(abs)

	cfg.applyDefaults()
	// 让"密钥 + 身份 + 工作内容全部写在配置里"：从密钥的 .pub / known_hosts 推导主机指纹
	cfg.expandKnownHostsKeys()
	cfg.expandFingerprintFiles()
	sortServerFingerprints(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// expandKnownHostsKeys 把 key.known_hosts（.pub 文件）里的公钥补进引用该密钥的服务器，
// 这样"密钥 + 身份 + 工作内容全部写在配置里"，不必手工抄指纹。
func (c *Config) expandKnownHostsKeys() {
	for i := range c.Keys {
		k := &c.Keys[i]
		if k.KnownHosts == "" {
			continue
		}
		data, err := os.ReadFile(k.KnownHosts)
		if err != nil {
			c.warnf("密钥 %q 的 known_hosts 文件读取失败: %v", k.Name, err)
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil {
				for j := range c.Servers {
					s := &c.Servers[j]
					if s.Key == k.Name && s.HostKey != nil {
						s.HostKey.Fingerprints = append(s.HostKey.Fingerprints, line)
					}
				}
			}
		}
	}
}

// sortServerFingerprints 去重并排序，保证输出稳定。
func sortServerFingerprints(c *Config) {
	for i := range c.Servers {
		hk := c.Servers[i].HostKey
		if hk == nil || len(hk.Fingerprints) == 0 {
			continue
		}
		seen := map[string]bool{}
		var out []string
		for _, fp := range hk.Fingerprints {
			if n, err := parseFingerprint(fp); err == nil {
				fp = n
			}
			if seen[fp] {
				continue
			}
			seen[fp] = true
			out = append(out, fp)
		}
		hk.Fingerprints = out
	}
}

// 展开 ${ENV}；未定义的变量保持原样，避免误伤密码里的 $。
func expandEnv(s string) string {
	return os.Expand(s, func(k string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return "${" + k + "}"
	})
}

// resolvePath 把配置里的路径解析为绝对路径：支持 ~ 与相对配置目录。
func (c *Config) resolvePath(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.dir, p)
	}
	return filepath.Clean(p)
}

func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Name == "" {
		c.Name = "sshex"
	}

	for i := range c.Keys {
		k := &c.Keys[i]
		k.Description = expandEnv(k.Description)
		if k.Path != "" {
			k.Path = c.resolvePath(expandEnv(k.Path))
		}
		if k.KnownHosts != "" {
			k.KnownHosts = c.resolvePath(expandEnv(k.KnownHosts))
		}

		raw, err := k.resolveType()
		if err == nil && raw == "password" {
			// 允许 value 写 "${PROD_PASSWORD}" 来引用环境变量
			k.Value = expandEnv(k.Value)
		}
		if raw == "agent" {
			k.Value = expandEnv(k.Value)
		}
	}

	if c.Identities.Default.Role == "" {
		c.Identities.Default.Role = "server"
	}

	for i := range c.Servers {
		s := &c.Servers[i]
		if s.Name == "" {
			s.Name = s.ID
		}
		if s.Port == 0 {
			s.Port = 22
		}
		if s.TimeoutSec <= 0 {
			s.TimeoutSec = 15
		}
		if s.Enabled == nil {
			v := true
			s.Enabled = &v
		}
		if s.HostKey == nil {
			s.HostKey = &HostKey{}
		}
		if s.HostKey.Policy == "" {
			s.HostKey.Policy = "strict"
		}
		s.HostKey.Policy = strings.ToLower(s.HostKey.Policy)
		if s.HostKey.KnownHosts == "" {
			// 放在配置文件旁边，不写到 ~/.ssh
			s.HostKey.KnownHosts = filepath.Join(c.dir, "known_hosts")
		} else {
			s.HostKey.KnownHosts = c.resolvePath(s.HostKey.KnownHosts)
		}
		if s.Jump != nil && s.Jump.Port == 0 {
			s.Jump.Port = 22
		}
		if s.Proxy != "" {
			s.Proxy = expandEnv(s.Proxy)
		}
	}

	for i := range c.Tasks {
		t := &c.Tasks[i]
		t.Type = strings.ToLower(t.Type)
		if t.Type == "" {
			t.Type = "command"
		}
		if t.ExitCode == nil {
			zero := 0
			t.ExitCode = &zero
		}
		if t.Enabled == nil {
			v := true
			t.Enabled = &v
		}
		if t.TimeoutSec <= 0 {
			t.TimeoutSec = 60
		}
		if t.Mode == "" {
			t.Mode = "0644"
		}
		if t.Overwrite == nil {
			v := false
			t.Overwrite = &v
		}
		if t.OutputFile != "" {
			t.OutputFile = c.resolvePath(t.OutputFile)
		}
	}

	if c.Policy.MaxParallel <= 0 {
		c.Policy.MaxParallel = 4
	}
	// policy.confirm_risk / continue_on_error 的默认值依赖"字段是否出现"，
	// 由 detectPolicyDefaults 依据原始 JSON 判断。

	if c.Alert.DiskUsedPercent == 0 {
		c.Alert.DiskUsedPercent = 85
	}
	if c.Alert.MemUsedPercent == 0 {
		c.Alert.MemUsedPercent = 90
	}
	if c.Alert.SwapUsedPercent == 0 {
		c.Alert.SwapUsedPercent = 50
	}
	if c.Alert.LoadPerCore == 0 {
		c.Alert.LoadPerCore = 2
	}

	if c.DataDir == "" {
		c.DataDir = filepath.Join(c.dir, "data")
	}
	c.DataDir = c.resolvePath(expandEnv(c.DataDir))
	c.Model.APIKey = expandEnv(c.Model.APIKey)
	c.Model.BaseURL = expandEnv(c.Model.BaseURL)
	if c.Report.Dir != "" {
		c.Report.Dir = c.resolvePath(c.Report.Dir)
	}
	if c.Policy.AuditLog != "" {
		c.Policy.AuditLog = c.resolvePath(c.Policy.AuditLog)
	}
}

// 由于 Policy.ConfirmRisk 默认应为 true，而 JSON 中未出现时是 false，
// 这里用原始 JSON 判断字段是否出现过。
func (c *Config) detectPolicyDefaults() {
	var raw struct {
		Policy map[string]json.RawMessage `json:"policy"`
	}
	if data, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(data, &raw)
	}
	if _, ok := raw.Policy["confirm_risk"]; !ok {
		c.Policy.ConfirmRisk = true
	}
	if _, ok := raw.Policy["continue_on_error"]; !ok {
		c.Policy.ContinueOnErr = true
	}
}

// KeyType 返回密钥实际类型。
func (k *Key) resolveType() (string, error) {
	t := strings.ToLower(strings.TrimSpace(k.Type))
	if t != "" {
		switch t {
		case "password", "private_key", "agent", "none":
			return t, nil
		case "ssh-key", "key", "privatekey", "pem":
			return "private_key", nil
		case "pass":
			return "password", nil
		}
		return "", fmt.Errorf("密钥 %q 的 type 不合法: %q（可选 password/private_key/agent/none）", k.Name, k.Type)
	}
	// 自动识别
	switch {
	case k.Path != "":
		return "private_key", nil
	case strings.Contains(k.Value, "PRIVATE KEY-----"):
		return "private_key", nil
	case strings.Contains(k.Value, "/") && strings.Contains(k.Value, "agent") && !strings.Contains(k.Value, " "):
		return "agent", nil
	case k.Value != "":
		return "password", nil
	}
	return "none", nil
}

// ============================================================================
// 校验
// ============================================================================

func (c *Config) validate() error {
	var errs []string
	var envWarn []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	c.detectPolicyDefaults()

	if c.Version != 1 {
		add("version 目前只支持 1，得到 %d", c.Version)
	}

	// keys
	keyNames := map[string]bool{}
	for i, k := range c.Keys {
		where := fmt.Sprintf("keys[%d]", i)
		if k.Name == "" {
			add("%s: name 不能为空", where)
			continue
		}
		where = fmt.Sprintf("keys[%d] (%s)", i, k.Name)
		if keyNames[k.Name] {
			add("%s: name 重复", where)
		}
		keyNames[k.Name] = true
		t, err := k.resolveType()
		if err != nil {
			add("%s: %v", where, err)
			continue
		}
		switch t {
		case "private_key":
			if k.Path == "" && k.Value == "" {
				add("%s: private_key 需要 path 或 value", where)
			}
			if k.Path != "" && k.Value == "" {
				if st, err := os.Stat(k.Path); err != nil {
					add("%s: 私钥文件不可读: %v", where, err)
				} else if st.IsDir() {
					add("%s: path 是目录而不是文件: %s", where, k.Path)
				} else if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
					add("%s: 私钥文件权限过于开放（%#o），请 chmod 600 %s", where, st.Mode().Perm(), k.Path)
				}
			}
		case "password":
			if k.Value == "" && k.Path == "" {
				add("%s: password 类型需要 value（可用 ${ENV} 引用环境变量）", where)
			}
			if m := undefinedEnvRef(k.Value); m != "" {
				// 环境变量未设置只是提示：dry-run/编辑器命令不需要真实密码
				envWarn = append(envWarn, fmt.Sprintf("密钥 %q 引用的环境变量 %s 当前未设置", k.Name, m))
			}
		}
	}

	// identities
	identIDs := map[string]bool{}
	for i, id := range c.Identities.Servers {
		if id.Name == "" {
			add("identities.servers[%d]: name（必须与 servers[].id 对应）不能为空", i)
			continue
		}
		if identIDs[id.Name] {
			add("identities.servers[%d]: name %q 重复", i, id.Name)
		}
		identIDs[id.Name] = true
	}

	// servers
	serverIDs := map[string]bool{}
	serverTags := map[string][]string{}
	for i, s := range c.Servers {
		where := fmt.Sprintf("servers[%d] (%s)", i, s.ID)
		if s.ID == "" {
			add("servers[%d]: id 不能为空", i)
			continue
		}
		if serverIDs[s.ID] {
			add("%s: id 重复", where)
		}
		serverIDs[s.ID] = true
		if s.Host == "" {
			add("%s: host 不能为空", where)
		}
		if s.Port <= 0 || s.Port > 65535 {
			add("%s: port 不合法: %d", where, s.Port)
		}
		if s.Key == "" {
			add("%s: 未指定 key（SSH 通常需要认证；如需免认证请配置一个 type=none 的密钥并引用它）", where)
		} else if _, ok := findKey(c.Keys, s.Key); !ok {
			add("%s: key %q 不存在", where, s.Key)
		}
		switch s.HostKey.Policy {
		case "strict", "accept-new", "insecure":
		default:
			add("%s: host_key.policy 不合法: %q（可选 strict/accept-new/insecure）", where, s.HostKey.Policy)
		}
		if s.HostKey.Verify == nil {
			v := true
			s.HostKey.Verify = &v
		}
		if s.HostKey.Policy == "strict" && len(s.HostKey.Fingerprints) == 0 && s.HostKey.KnownHosts == "" {
			add("%s: strict 策略需要 fingerprints 或 known_hosts", where)
		}
		for _, fp := range s.HostKey.Fingerprints {
			if _, err := s.HostKey.resolveFingerprint(fp); err != nil {
				add("%s: %v", where, err)
			}
		}
		proxySet := 0
		if s.Jump != nil {
			proxySet++
		}
		if s.Proxy != "" {
			proxySet++
		}
		if len(s.ProxyTokens) > 0 {
			proxySet++
		}
		if proxySet > 1 {
			add("%s: jump / proxy_command / proxy_tokens 只能配置一种", where)
		}
		if s.Jump != nil {
			j := s.Jump
			if j.Server == "" && j.Host == "" {
				add("%s: jump 需要 server 或 host", where)
			}
			if j.Server != "" && j.Host != "" {
				add("%s: jump.server 与 jump.host 只能二选一", where)
			}
			if j.Host != "" && j.Key == "" {
				add("%s: 跳板机直接给 host 时必须在 jump.key 指定密钥", where)
			}
			if j.Key != "" {
				if _, ok := findKey(c.Keys, j.Key); !ok {
					add("%s: jump.key %q 不存在", where, j.Key)
				}
			}
		}
		if id, ok := findIdentity(c, s.ID); ok {
			for _, t := range id.Tags {
				serverTags[t] = append(serverTags[t], s.ID)
			}
		}
		for _, t := range s.Tags {
			serverTags[t] = append(serverTags[t], s.ID)
		}
	}
	// jump.server 引用的必须是已定义服务器
	for i, s := range c.Servers {
		if s.Jump != nil && s.Jump.Server != "" {
			if !serverIDs[s.Jump.Server] {
				add("servers[%d] (%s): jump.server %q 不存在", i, s.ID, s.Jump.Server)
			}
			if s.Jump.Server == s.ID {
				add("servers[%d] (%s): 不能把自己作为跳板机", i, s.ID)
			}
		}
		// identities 引用了不存在的服务器只警告（体检时检查），不阻断
	}

	// 跳板机环路检测
	if cycle := detectJumpCycle(c); cycle != "" {
		add("jump 配置存在环路: %s", cycle)
	}

	// tasks
	taskNames := map[string]bool{}
	for i, t := range c.Tasks {
		where := fmt.Sprintf("tasks[%d] (%s)", i, t.Name)
		if t.Name == "" {
			add("tasks[%d]: name 不能为空", i)
			continue
		}
		if taskNames[t.Name] {
			add("%s: name 重复", where)
		}
		taskNames[t.Name] = true
		switch t.Type {
		case "command":
			if strings.TrimSpace(t.Command) == "" {
				add("%s: command 不能为空", where)
			}
		case "upload":
			if t.Local == "" || t.Remote == "" {
				add("%s: upload 需要 local 和 remote", where)
			}
			if t.Local != "" && t.Type == "upload" {
				p := c.resolvePath(t.Local)
				if st, err := os.Stat(p); err != nil {
					add("%s: 本地文件不存在: %s", where, p)
				} else if st.IsDir() {
					add("%s: local 目前只支持文件，不支持目录: %s", where, p)
				}
			}
		case "download":
			if t.Local == "" || t.Remote == "" {
				add("%s: download 需要 local 和 remote", where)
			}
		default:
			add("%s: type 不合法 %q（可选 command/upload/download）", where, t.Type)
		}
		if t.OutputFile != "" && t.Type != "command" {
			add("%s: output_file 只对 type=command 有效", where)
		}
		if t.Inspect != nil {
			if t.Type != "command" {
				add("%s: inspect 只对 type=command 有效", where)
			}
			switch t.Inspect.Kind {
			case "df", "meminfo", "load":
			default:
				add("%s: inspect.kind 不合法 %q（可选 df/meminfo/load）", where, t.Inspect.Kind)
			}
		}
		for _, h := range t.Hosts {
			if !serverIDs[h] {
				add("%s: hosts 引用了不存在的服务器 %q", where, h)
			}
		}
		if _, err := parseMode(t.Mode); err != nil {
			add("%s: %v", where, err)
		}
	}

	// model 段：允许为空（不用 AI 功能），一旦填了就要求基本完整
	if c.Model.BaseURL != "" || c.Model.Model != "" || c.Model.APIKey != "" {
		if c.Model.BaseURL == "" {
			add("model.base_url 不能为空（例如 https://api.deepseek.com/v1）")
		}
		if c.Model.Model == "" {
			add("model.model 不能为空（例如 deepseek-flash、gpt-4o-mini、qwen2.5）")
		}
		// api_key 里 ${ENV} 未设置的情况留给 chat 引导处理，这里不阻断
		if c.Model.APIKey == "" && undefinedEnvRef(c.Model.APIKey) == "" && !isLocalEndpoint(c.Model.BaseURL) {
			add("model.api_key 不能为空（本地 Ollama/vLLM 等无需 Key 的接口会自动豁免）")
		}
	}

	if c.Policy.MaxParallel < 1 {
		add("policy.max_parallel 至少为 1")
	}
	if c.Policy.MaxParallel > 64 {
		add("policy.max_parallel 过大（>64），请调小以免压垮目标机")
	}
	for _, p := range c.Policy.DenyPatterns {
		if err := checkRegexp(p); err != nil {
			add("policy.deny_patterns 里的正则 %q 不合法: %v", p, err)
		}
	}
	for _, p := range c.Policy.AllowPatterns {
		if err := checkRegexp(p); err != nil {
			add("policy.allow_patterns 里的正则 %q 不合法: %v", p, err)
		}
	}

	if len(errs) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "配置校验失败（%d 处问题）:\n", len(errs))
		for _, e := range errs {
			fmt.Fprintf(&b, "  - %s\n", e)
		}
		b.WriteString("\n提示: 运行 `sshex check` 可以查看以上全部问题。")
		return errors.New(b.String())
	}

	// 非致命警告：收集起来，由调用方决定是否输出（check/inv 等命令会展示）
	c.warnings = append(c.warnings, envWarn...)
	for id := range identIDs {
		if !serverIDs[id] {
			c.warnings = append(c.warnings, fmt.Sprintf("identities.servers 里介绍了 %q，但没有同 id 的服务器", id))
		}
	}
	for _, s := range c.Servers {
		if _, ok := findIdentity(c, s.ID); !ok {
			c.warnings = append(c.warnings, fmt.Sprintf("服务器 %q 没有身份介绍（role/owner/description），建议在 identities.servers 中补上", s.ID))
		}
	}
	sortStrings(c.warnings)
	return nil
}

// warnf 输出到 stderr，不打断流程。
func (c *Config) warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "警告: "+format+"\n", a...)
}

// Warnings 返回加载时收集的非致命问题。
func (c *Config) Warnings() []string { return c.warnings }

func sortStrings(s []string) {
	sort.Strings(s)
}

func findKey(keys []Key, name string) (*Key, bool) {
	for i := range keys {
		if keys[i].Name == name {
			return &keys[i], true
		}
	}
	return nil, false
}

func findIdentity(c *Config, id string) (Identity, bool) {
	for _, x := range c.Identities.Servers {
		if x.Name == id {
			return x, true
		}
	}
	return Identity{}, false
}

func detectJumpCycle(c *Config) string {
	byID := map[string]*Server{}
	for i := range c.Servers {
		byID[c.Servers[i].ID] = &c.Servers[i]
	}
	for _, s := range c.Servers {
		seen := map[string]bool{s.ID: true}
		cur := s
		var chain []string
		chain = append(chain, s.ID)
		for cur.Jump != nil && cur.Jump.Server != "" {
			nxt, ok := byID[cur.Jump.Server]
			if !ok {
				break
			}
			chain = append(chain, nxt.ID)
			if seen[nxt.ID] {
				return strings.Join(chain, " -> ")
			}
			seen[nxt.ID] = true
			cur = *nxt
		}
	}
	return ""
}

// ============================================================================
// 查询辅助
// ============================================================================

// EffectiveIdentity 合并默认身份与服务器自身介绍，并补齐来自 server 的字段。
func (c *Config) EffectiveIdentity(s *Server) Identity {
	id := c.Identities.Default
	if own, ok := findIdentity(c, s.ID); ok {
		if own.Role != "" {
			id.Role = own.Role
		}
		if own.Owner != "" {
			id.Owner = own.Owner
		}
		if own.Description != "" {
			id.Description = own.Description
		}
		id.Tags = union(nil, own.Tags)
		id.Notes = union(nil, own.Notes)
	}
	if s.Role != "" {
		id.Role = s.Role
	}
	if s.Owner != "" {
		id.Owner = s.Owner
	}
	if s.Desc != "" {
		id.Description = s.Desc
	}
	id.Tags = union(id.Tags, s.Tags)
	id.Notes = union(id.Notes, s.Notes)
	return id
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{a, b} {
		for _, v := range list {
			v = strings.TrimSpace(v)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

// matchAny 判断输入是否命中列表中的任意一项（不区分大小写）。
func matchAny(list []string, input string) bool {
	return containsFold(list, input)
}

func truthy(v *bool) bool { return v == nil || *v }

// isLocalEndpoint 判断是否本机/局域网模型服务（这类服务通常不需要 API Key）。
func isLocalEndpoint(baseURL string) bool {
	u := strings.ToLower(baseURL)
	for _, host := range []string{"localhost", "127.0.0.1", "0.0.0.0", "::1", "host.docker.internal"} {
		if strings.Contains(u, host) {
			return true
		}
	}
	return false
}

// undefinedEnvRef 找出字符串里未被定义的 ${VAR}，返回变量名。
func undefinedEnvRef(v string) string {
	missing := ""
	os.Expand(v, func(key string) string {
		if _, ok := os.LookupEnv(key); !ok && missing == "" {
			missing = key
		}
		return ""
	})
	return missing
}

// Mask 把秘密打码，只保留首尾极少量字符，用于日志和输出。
func Mask(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= 4 {
		return "****"
	}
	return string(r[:1]) + "****" + string(r[len(r)-1:])
}

// sha256Short 给值做一个短摘要，便于在输出中区分而不泄露。
func sha256Short(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawStdEncoding.EncodeToString(sum[:6])
}

func shortHost(host string, port int) string {
	if strings.Contains(host, ":") && strings.Count(host, ":") > 1 {
		return net.JoinHostPort(host, strconv.Itoa(port))
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func durSec(n int) time.Duration { return time.Duration(n) * time.Second }

// parseMode 解析 "0644" / "755" / "0o755" / "0755" 形式的权限。
func parseMode(s string) (os.FileMode, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0o"), "0O")
	if s == "" {
		return 0o644, nil
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil || n > 0o7777 {
		return 0, fmt.Errorf("mode 不合法 %q，应为八进制权限如 \"0644\"", s)
	}
	return os.FileMode(n), nil
}

// checkRegexp 校验正则是否合法。
func checkRegexp(p string) error {
	_, err := regexp.Compile(p)
	return err
}

// serverByID 按 id 查找服务器。
func (c *Config) serverByID(id string) *Server {
	for i := range c.Servers {
		if strings.EqualFold(c.Servers[i].ID, id) {
			return &c.Servers[i]
		}
	}
	return nil
}

// resolveFingerprint 把 fingerprints 中的一项解析为公钥行：
// 既支持 "SHA256:..." 指纹、authorized_keys 格式公钥，
// 也支持"公钥文件路径"（如 /REPLACE/ME/host-key.pub 或 ~/.ssh/id.pub）。
func (h *HostKey) resolveFingerprint(entry string) (string, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", fmt.Errorf("主机指纹不能为空")
	}
	if strings.HasPrefix(strings.ToLower(entry), "sha256:") {
		n, err := parseFingerprint(entry)
		if err != nil {
			return "", fmt.Errorf("主机指纹格式错误 %q: %v", entry, err)
		}
		return n, nil
	}
	// 公钥行本身
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(entry)); err == nil {
		return entry, nil
	}
	// 当作公钥文件路径
	path := entry
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("主机指纹 %q 既不是 SHA256 指纹/公钥行，也不是可读的公钥文件: %v", entry, err)
	}
	line := strings.TrimSpace(string(data))
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err != nil {
		return "", fmt.Errorf("公钥文件 %s 内容不是合法的公钥: %v", path, err)
	}
	return line, nil
}

// expandFingerprintFiles 在加载配置时把 fingerprints 里的公钥文件路径替换成公钥内容。
func (c *Config) expandFingerprintFiles() {
	for i := range c.Servers {
		hk := c.Servers[i].HostKey
		if hk == nil || len(hk.Fingerprints) == 0 {
			continue
		}
		var out []string
		for _, fp := range hk.Fingerprints {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(fp)), "sha256:") {
				out = append(out, fp)
				continue
			}
			if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(fp)); err == nil {
				out = append(out, fp)
				continue
			}
			// 相对路径按配置文件目录解析
			path := fp
			if !filepath.IsAbs(path) && !strings.HasPrefix(path, "~") {
				path = filepath.Join(c.dir, path)
			}
			if strings.HasPrefix(path, "~") {
				if home, err := os.UserHomeDir(); err == nil {
					path = filepath.Join(home, strings.TrimPrefix(path, "~"))
				}
			}
			if data, err := os.ReadFile(path); err == nil {
				out = append(out, strings.TrimSpace(string(data)))
			} else {
				out = append(out, fp) // 保留原样，由 validate 报错
			}
		}
		hk.Fingerprints = out
	}
}
