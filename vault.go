package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 服务器凭据存储
//
// 每台服务器一个独立 JSON 文件，放在 hosts/ 目录下：
//   hosts/10.0.1.11.json
//
// 按你的要求不加密，但文件权限设为 0600（仅本人可读写）。
// 文件里同时保存"这台机器是干什么的"，这样用久了也知道哪台是哪个。
// ============================================================================

// HostRecord 是一台被管理的服务器的完整信息（IP + 密码 + 用途记忆）。
type HostRecord struct {
	ID          string   `json:"id"`       // 唯一标识，默认用 IP
	IP          string   `json:"ip"`       // 必填，IP 或域名
	Port        int      `json:"port"`     // 默认 22
	Username    string   `json:"username"` // 登录用户
	Password    string   `json:"password"` // 登录密码（不加密存储）
	KeyFile     string   `json:"key_file"` // 也可用私钥登录
	Purpose     string   `json:"purpose"`  // 这台机器干什么的：web / db / 测试机 ...
	Owner       string   `json:"owner"`    // 负责人
	Tags        []string `json:"tags"`     // 标签
	Notes       []string `json:"notes"`    // 备注、注意事项、踩过的坑
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	LastUsedAt  string   `json:"last_used_at"`
	LastSummary string   `json:"last_summary"` // 最近一次运维结论（由 AI 总结）
}

// HostStore 管理 hosts/ 目录。
type HostStore struct {
	dir string
	mu  sync.Mutex
}

func NewHostStore(dir string) (*HostStore, error) {
	if dir == "" {
		return nil, errors.New("hosts 目录为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &HostStore{dir: dir}, nil
}

// Dir 返回存储目录。
func (s *HostStore) Dir() string { return s.dir }

// fileFor 生成主机对应的文件名（用 IP/标识做文件名，替换不安全字符）。
func (s *HostStore) fileFor(id string) string {
	return filepath.Join(s.dir, sanitizeName(id)+".json")
}

// List 读取全部服务器记录，按用途/ID 排序。
func (s *HostStore) List() ([]HostRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []HostRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var rec HostRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			fmt.Fprintf(os.Stderr, "警告: 跳过损坏的服务器文件 %s: %v\n", e.Name(), err)
			continue
		}
		if rec.ID == "" {
			rec.ID = strings.TrimSuffix(e.Name(), ".json")
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Purpose != out[j].Purpose {
			return out[i].Purpose < out[j].Purpose
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Find 按 ID、IP 或 IP:端口 查找（唯一命中才算找到）。
func (s *HostStore) Find(name string) (*HostRecord, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	var hits []*HostRecord
	host, port := splitHostPort(name)
	for i := range all {
		r := &all[i]
		switch {
		case strings.EqualFold(r.ID, name):
			return r, nil
		case r.IP == host && (port == 0 || r.Port == port):
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("没有找到 %q 对应的服务器（可用 'list' 查看现有服务器）", name)
	case 1:
		return hits[0], nil
	default:
		return nil, fmt.Errorf("%q 匹配到多台服务器（端口不同），请用 主机:端口 指定", name)
	}
}

func splitHostPort(v string) (string, int) {
	if i := strings.LastIndex(v, ":"); i > 0 && !strings.Contains(v[:i], ":") {
		if p := atoiLoose(v[i+1:]); p > 0 {
			return v[:i], p
		}
	}
	return v, 0
}

func atoiLoose(s string) int {
	n := 0
	if s == "" {
		return 0
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// Save 写入（新建或覆盖）一条记录，文件权限 0600。
func (s *HostStore) Save(rec *HostRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Format(time.RFC3339)
	if rec.ID == "" {
		rec.ID = rec.IP
	}
	rec.Port = portOrDefault(rec.Port)
	if rec.CreatedAt == "" {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now

	path := s.fileFor(rec.ID)
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	// 先写临时文件再改名，避免写一半损坏
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入服务器文件失败: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = err
	}
	return os.Rename(tmp, path)
}

// Delete 删除一条记录。
func (s *HostStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.fileFor(id)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("服务器 %q 不存在", id)
	}
	return os.Remove(path)
}

// Touch 记录"最近一次使用时间"。
func (s *HostStore) Touch(rec *HostRecord) {
	rec.LastUsedAt = time.Now().Format(time.RFC3339)
	_ = s.Save(rec)
}

// MergeNotes 追加备注（去重，最多保留 20 条，避免文件无限膨胀）。
func (s *HostStore) MergeNotes(rec *HostRecord, notes []string) {
	seen := map[string]bool{}
	var out []string
	for _, n := range append(append([]string{}, rec.Notes...), notes...) {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) > 20 {
		out = out[len(out)-20:]
	}
	rec.Notes = out
}

func portOrDefault(p int) int {
	if p <= 0 {
		return 22
	}
	return p
}

// Addr 返回 "user@ip:port" 形式的展示地址。
func (r *HostRecord) Addr() string {
	user := r.Username
	if user == "" {
		user = "?"
	}
	return fmt.Sprintf("%s@%s", user, shortHost(r.IP, portOrDefault(r.Port)))
}

// BuildSSHConfig 把这些记录转成 sshex 内部的 Config（用于复用已有的 SSH 层）。
func (s *HostStore) BuildSSHConfig(cfg *Config, rec *HostRecord) (*Config, *Server, error) {
	out := *cfg
	out.Servers = []Server{{
		ID:       rec.ID,
		Name:     rec.ID,
		Host:     rec.IP,
		Port:     portOrDefault(rec.Port),
		Username: rec.Username,
		Key:      "vault_" + rec.ID,
		Desc:     rec.Purpose,
		Owner:    rec.Owner,
		Tags:     rec.Tags,
		// 凭据已由用户在本机交互录入，指纹策略用 accept-new 免去首次确认的摩擦，
		// 但仍会校验已记录的主机密钥变化。
		HostKey: &HostKey{Policy: "accept-new", KnownHosts: cfg.sshKnownHostsPath()},
		Enabled: boolPtr(true),
	}}
	// 构造密钥对象
	var key Key
	switch {
	case rec.KeyFile != "":
		key = Key{Name: "vault_" + rec.ID, Type: "private_key", Path: rec.KeyFile}
	case rec.Password != "":
		key = Key{Name: "vault_" + rec.ID, Type: "password", Value: rec.Password}
	default:
		key = Key{Name: "vault_" + rec.ID, Type: "agent", Value: os.Getenv("SSH_AUTH_SOCK")}
	}
	if key.Type == "agent" && key.Value == "" {
		return nil, nil, fmt.Errorf("服务器 %s 没有密码/私钥，且本机没有运行 ssh-agent", rec.ID)
	}
	out.Keys = []Key{key}
	out.Identities = IdentityBook{
		Default: Identity{},
		Servers: []Identity{{
			Name:        rec.ID,
			Role:        rec.Purpose,
			Owner:       rec.Owner,
			Tags:        rec.Tags,
			Description: rec.Purpose,
			Notes:       rec.Notes,
		}},
	}
	// 让 applyDefaults 生效
	if err := out.validateForVault(); err != nil {
		return nil, nil, err
	}
	return &out, &out.Servers[0], nil
}

// validateForVault 对单机配置补齐必需字段（不跑完整校验，避免警告噪音）。
func (c *Config) validateForVault() error {
	if len(c.Servers) == 0 {
		return errors.New("没有服务器")
	}
	s := &c.Servers[0]
	if s.HostKey == nil {
		s.HostKey = &HostKey{Policy: "accept-new"}
	}
	if s.HostKey.Verify == nil {
		s.HostKey.Verify = boolPtr(true)
	}
	if s.Key == "" {
		return errors.New("缺少密钥")
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }

// sshKnownHostsPath 返回 known_hosts 路径：始终放在数据目录里，
// 绝不写用户主目录下的 ~/.ssh。
func (c *Config) sshKnownHostsPath() string {
	if c != nil && c.DataDir != "" {
		return filepath.Join(c.DataDir, "known_hosts")
	}
	return filepath.Join(dataHome(), "known_hosts")
}

// Describe 生成给 AI 看的一行摘要。
func (r *HostRecord) Describe() string {
	parts := []string{fmt.Sprintf("%s (%s)", r.ID, r.Addr())}
	if r.Purpose != "" {
		parts = append(parts, "用途: "+r.Purpose)
	}
	if r.Owner != "" {
		parts = append(parts, "负责人: "+r.Owner)
	}
	if len(r.Tags) > 0 {
		parts = append(parts, "标签: "+strings.Join(r.Tags, ","))
	}
	if r.LastSummary != "" {
		parts = append(parts, "最近结论: "+firstLine(r.LastSummary))
	}
	return strings.Join(parts, " | ")
}
