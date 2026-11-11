package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 长期记忆
//
// 目的：用久了还能知道"哪台服务器是干什么的、之前出过什么事、结论是什么"。
//
// 两层存储：
//   1. servers/<id>.json  —— 机器级记忆：用途、负责人、备注、最近结论（vault.go）
//   2. MEMORY.md          —— 全局记忆：按时间追加的运维结论，人类可直接阅读
//   3. history.jsonl      —— 原始操作流水（可选，供 AI 回溯细节）
// ============================================================================

// Memory 是记忆文件管理器。
type Memory struct {
	DataDir  string
	MemoryMD string // MEMORY.md 路径
	History  string // history.jsonl 路径
	mu       sync.Mutex
}

// NewMemory 初始化记忆存储（自动创建目录与 MEMORY.md 骨架）。
func NewMemory(dataDir string) (*Memory, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("data_dir 为空")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	m := &Memory{
		DataDir:  dataDir,
		MemoryMD: filepath.Join(dataDir, "MEMORY.md"),
		History:  filepath.Join(dataDir, "history.jsonl"),
	}
	if _, err := os.Stat(m.MemoryMD); os.IsNotExist(err) {
		if err := os.WriteFile(m.MemoryMD, []byte(memoryHeader()), 0o600); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func memoryHeader() string {
	return "# 运维记忆\n\n" +
		"> 本文件由 AI 助手自动维护，记录每次运维的结论与重要发现。\n" +
		"> 可以直接用文本编辑器修改，AI 下次会读到你的改动。\n\n" +
		"## 长期约定\n\n" +
		"（在这里写所有操作都要遵守的规则，例如：生产变更前必须先备份；禁止在高峰期重启）\n\n" +
		"---\n\n"
}

// Count 返回已记录的运维结论条数。
func (m *Memory) Count() int {
	data, err := os.ReadFile(m.MemoryMD)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		// 只统计带日期的运维记录，不把"长期约定"算进去
		if strings.HasPrefix(line, "## ") && !strings.HasPrefix(line, "## 长期约定") {
			n++
		}
	}
	return n
}

// ReadAll 读取 MEMORY.md 全文。
func (m *Memory) ReadAll() (string, error) {
	data, err := os.ReadFile(m.MemoryMD)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Tail 返回 MEMORY.md 的最后 n 个字符（避免把整份文件塞进上下文）。
func (m *Memory) Tail(maxBytes int) string {
	data, err := os.ReadFile(m.MemoryMD)
	if err != nil {
		return ""
	}
	if len(data) <= maxBytes {
		return string(data)
	}
	s := string(data[len(data)-maxBytes:])
	// 从下一个换行处开始，避免截断半行
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return "（较早的记忆已省略）\n" + s
}

// MemoryEntry 是一条要写入的运维记忆。
type MemoryEntry struct {
	Time     time.Time
	Hosts    []string // 涉及的服务器
	Summary  string   // 一句话结论
	Details  []string // 关键发现/待办
	Commands []string // 执行过的命令（可选，供回溯）
}

// Append 把结论追加到 MEMORY.md，并写一条历史流水。
func (m *Memory) Append(e MemoryEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	summary := strings.TrimSpace(e.Summary)
	if summary == "" {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## %s", e.Time.Format("2006-01-02 15:04"))
	if len(e.Hosts) > 0 {
		fmt.Fprintf(&b, " · %s", strings.Join(e.Hosts, ", "))
	}
	b.WriteString("\n\n")
	b.WriteString(summary)
	b.WriteString("\n")
	for _, d := range e.Details {
		d = strings.TrimSpace(d)
		if d != "" {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}
	if len(e.Commands) > 0 {
		b.WriteString("\n<details><summary>执行记录</summary>\n\n```\n")
		for _, c := range e.Commands {
			b.WriteString(strings.TrimSpace(c))
			b.WriteString("\n")
		}
		b.WriteString("```\n\n</details>\n")
	}
	b.WriteString("\n")

	f, err := os.OpenFile(m.MemoryMD, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// 历史流水（JSON Lines），方便 AI 之后按主机检索
	line := fmt.Sprintf(`{"time":%q,"hosts":%q,"summary":%q}`,
		e.Time.Format(time.RFC3339), strings.Join(e.Hosts, ","), summary)
	if hf, err := os.OpenFile(m.History, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		hf.WriteString(line + "\n")
		hf.Close()
	}
	return nil
}

// RecentEntries 从 MEMORY.md 中取出指定主机的最近若干条记忆（按二级标题切分）。
func (m *Memory) RecentEntries(host string, limit int) []string {
	data, err := os.ReadFile(m.MemoryMD)
	if err != nil {
		return nil
	}
	sections := splitMemorySections(string(data))
	var hits []string
	for i := len(sections) - 1; i >= 0 && len(hits) < limit; i-- {
		sec := sections[i]
		if strings.Contains(sec, host) {
			hits = append([]string{strings.TrimSpace(sec)}, hits...)
		}
	}
	return hits
}

// splitMemorySections 按 "## " 切分。
func splitMemorySections(text string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "## ") {
			if cur.Len() > 0 {
				out = append(out, cur.String())
			}
			cur.Reset()
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ============================================================================
// AI 生成的记忆文本清洗
// ============================================================================

var memoryMarker = regexp.MustCompile(`(?s)<memory>(.*?)</memory>`)

// ExtractMemory 从 AI 回复里抽取 <memory>…</memory> 段落。
// 结构：
//
//	summary: 一句话结论
//	host: web-01
//	detail: 关键发现
//	note: 要长期记住的备注
func ExtractMemory(reply string) (summary string, hosts []string, details []string, notes []string, ok bool) {
	ms := memoryMarker.FindAllStringSubmatch(reply, -1)
	if len(ms) == 0 {
		return "", nil, nil, nil, false
	}
	for _, m := range ms {
		for _, line := range strings.Split(m[1], "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			key, val, found := strings.Cut(line, ":")
			if !found {
				key, val, found = strings.Cut(line, "：")
			}
			val = strings.TrimSpace(val)
			if !found || val == "" {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "summary", "结论":
				summary = val
			case "host", "server", "主机", "服务器":
				for _, h := range strings.FieldsFunc(val, func(r rune) bool { return r == ',' || r == '，' || r == ' ' }) {
					hosts = append(hosts, h)
				}
			case "detail", "发现", "详情":
				details = append(details, val)
			case "note", "备注", "记住":
				notes = append(notes, val)
			}
		}
	}
	return summary, hosts, details, notes, true
}

// StripMemoryTag 从展示给用户的文本里去掉 <memory> 块。
func StripMemoryTag(reply string) string {
	out := memoryMarker.ReplaceAllString(reply, "")
	return strings.TrimSpace(out)
}
