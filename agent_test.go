package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
// 服务器凭据文件（vault）
// ============================================================================

func TestHostStoreSaveAndFind(t *testing.T) {
	dir := t.TempDir()
	st, err := NewHostStore(filepath.Join(dir, "servers"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &HostRecord{ID: "web-01", IP: "10.0.1.11", Port: 22, Username: "root",
		Password: "p@ss", Purpose: "生产 web，跑 Nginx", Tags: []string{"prod"}}
	if err := st.Save(rec); err != nil {
		t.Fatal(err)
	}

	// 文件权限必须是 0600
	st2, err := os.Stat(filepath.Join(dir, "servers", "web-01.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st2.Mode().Perm(); perm != 0o600 {
		t.Errorf("服务器文件权限应为 0600，得到 %#o", perm)
	}

	// 按 ID 找
	got, err := st.Find("web-01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "p@ss" || got.Purpose == "" {
		t.Errorf("内容不一致: %+v", got)
	}
	// 按 IP 找
	if _, err := st.Find("10.0.1.11"); err != nil {
		t.Errorf("应能按 IP 找到: %v", err)
	}
	// 端口带 IP
	if _, err := st.Find("10.0.1.11:22"); err != nil {
		t.Errorf("应能按 IP:端口 找到: %v", err)
	}
	// 找不到
	if _, err := st.Find("nope"); err == nil {
		t.Error("不存在的服务器应报错")
	}
}

func TestHostStoreMultiplePorts(t *testing.T) {
	st, _ := NewHostStore(t.TempDir())
	st.Save(&HostRecord{ID: "a", IP: "10.0.0.1", Port: 22, Username: "root"})
	st.Save(&HostRecord{ID: "b", IP: "10.0.0.1", Port: 2222, Username: "root"})

	if _, err := st.Find("10.0.0.1"); err == nil {
		t.Error("同 IP 不同端口应提示歧义而不是随便选一台")
	}
	if r, err := st.Find("10.0.0.1:2222"); err != nil || r.ID != "b" {
		t.Errorf("应精确匹配端口: %v %v", r, err)
	}
}

func TestHostStoreDelete(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewHostStore(dir)
	st.Save(&HostRecord{ID: "x", IP: "1.1.1.1", Username: "root"})
	if err := st.Delete("x"); err != nil {
		t.Fatal(err)
	}
	hosts, _ := st.List()
	if len(hosts) != 0 {
		t.Errorf("删除后应为空，得到 %d", len(hosts))
	}
	if err := st.Delete("x"); err == nil {
		t.Error("删除不存在的服务器应报错")
	}
}

func TestHostStoreCorruptFile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{oops"), 0o600)
	st, _ := NewHostStore(dir)
	hosts, err := st.List()
	if err != nil {
		t.Fatalf("损坏文件不应让整个列表失败: %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("损坏文件应被跳过，得到 %d", len(hosts))
	}
}

func TestMergeNotesDedupAndCap(t *testing.T) {
	st, _ := NewHostStore(t.TempDir())
	rec := &HostRecord{}
	st.MergeNotes(rec, []string{"a", "a", " ", "b"})
	if len(rec.Notes) != 2 || rec.Notes[0] != "a" || rec.Notes[1] != "b" {
		t.Errorf("去重失败: %v", rec.Notes)
	}
	var many []string
	for i := 0; i < 30; i++ {
		many = append(many, string(rune('a'+i%26))+string(rune('0'+i/10)))
	}
	st.MergeNotes(rec, many)
	if len(rec.Notes) > 20 {
		t.Errorf("备注应限制在 20 条，得到 %d", len(rec.Notes))
	}
	// 保留的是最新的
	if rec.Notes[len(rec.Notes)-1] != many[len(many)-1] {
		t.Errorf("应保留最新备注: %v", rec.Notes[len(rec.Notes)-1])
	}
}

func TestBuildSSHConfig(t *testing.T) {
	st, _ := NewHostStore(t.TempDir())
	cfg := &Config{}
	rec := &HostRecord{ID: "h1", IP: "10.1.1.1", Port: 2222, Username: "ops", Password: "pw", Purpose: "db"}
	sub, srv, err := st.BuildSSHConfig(cfg, rec)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Host != "10.1.1.1" || srv.Port != 2222 || srv.Username != "ops" {
		t.Errorf("服务器字段错误: %+v", srv)
	}
	if len(sub.Keys) != 1 || sub.Keys[0].Value != "pw" {
		t.Errorf("密码应传给 SSH 层: %+v", sub.Keys)
	}
	// 没有密码也没有 ssh-agent 时报错
	os.Unsetenv("SSH_AUTH_SOCK")
	rec2 := &HostRecord{ID: "h2", IP: "10.1.1.2", Username: "ops"}
	if _, _, err := st.BuildSSHConfig(cfg, rec2); err == nil {
		t.Error("无凭据时应报错")
	}
}

// ============================================================================
// 记忆
// ============================================================================

func TestMemoryAppend(t *testing.T) {
	dir := t.TempDir()
	m, err := NewMemory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.MemoryMD); err != nil {
		t.Fatal("应自动创建 MEMORY.md")
	}
	err = m.Append(MemoryEntry{
		Hosts:    []string{"web-01"},
		Summary:  "web-01 磁盘 91%",
		Details:  []string{"/ 分区 91%"},
		Commands: []string{"df -h /"},
	})
	if err != nil {
		t.Fatal(err)
	}
	all, _ := m.ReadAll()
	for _, want := range []string{"web-01", "磁盘 91%", "/ 分区 91%", "df -h /"} {
		if !strings.Contains(all, want) {
			t.Errorf("MEMORY.md 缺少 %q", want)
		}
	}
	// history.jsonl
	h, _ := os.ReadFile(m.History)
	if !strings.Contains(string(h), "web-01 磁盘 91%") {
		t.Errorf("历史流水缺失: %s", h)
	}
	// 空 summary 不写入
	before := len(all)
	m.Append(MemoryEntry{Hosts: []string{"x"}})
	after, _ := m.ReadAll()
	if len(after) != before {
		t.Error("空结论不应写入")
	}
}

func TestMemoryTailAndRecent(t *testing.T) {
	m, _ := NewMemory(t.TempDir())
	m.Append(MemoryEntry{Hosts: []string{"db-01"}, Summary: "主库负载高"})
	m.Append(MemoryEntry{Hosts: []string{"web-01"}, Summary: "web 正常"})

	tail := m.Tail(200)
	if !strings.Contains(tail, "web 正常") {
		t.Errorf("Tail 应包含最新内容: %q", tail)
	}
	recent := m.RecentEntries("db-01", 5)
	if len(recent) != 1 || !strings.Contains(recent[0], "主库负载高") {
		t.Errorf("按主机检索失败: %v", recent)
	}
}

func TestExtractMemory(t *testing.T) {
	reply := `磁盘有点满。

<memory>
summary: web-01 磁盘 / 使用率 91%
host: web-01, db-01
detail: /var/log 占 30G
note: 日志增长较快
</memory>
以上是结论。`
	summary, hosts, details, notes, ok := ExtractMemory(reply)
	if !ok || summary != "web-01 磁盘 / 使用率 91%" {
		t.Fatalf("解析失败: %v %q", ok, summary)
	}
	if len(hosts) != 2 || hosts[0] != "web-01" || hosts[1] != "db-01" {
		t.Errorf("主机解析错误: %v", hosts)
	}
	if len(details) != 1 || len(notes) != 1 {
		t.Errorf("明细/备注解析错误: %v %v", details, notes)
	}
	// 展示文本应去掉记忆块
	shown := StripMemoryTag(reply)
	if strings.Contains(shown, "<memory>") || !strings.Contains(shown, "磁盘有点满") {
		t.Errorf("展示文本处理错误: %q", shown)
	}
	// 中文冒号也支持
	_, _, _, _, ok2 := ExtractMemory("<memory>\n结论：某机器正常\n</memory>")
	if !ok2 {
		t.Error("中文冒号应能解析")
	}
	// 没有记忆块
	if _, _, _, _, ok3 := ExtractMemory("普通回复"); ok3 {
		t.Error("没有记忆块时 ok 应为 false")
	}
}

// ============================================================================
// Agent 工具逻辑（不连网络）
// ============================================================================

// fakeUI 记录交互过程，按预设顺序回答问题。
type fakeUI struct {
	answers []string
	asked   []string
	out     strings.Builder
}

func (f *fakeUI) Println(a ...any) {
	for _, v := range a {
		f.out.WriteString(fmt.Sprint(v))
		f.out.WriteString("\n")
	}
}
func (f *fakeUI) Printf(format string, a ...any) {
	f.out.WriteString(fmt.Sprintf(format, a...))
}
func (f *fakeUI) next(prompt string) (string, error) {
	f.asked = append(f.asked, prompt)
	if len(f.answers) == 0 {
		return "", nil
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a, nil
}
func (f *fakeUI) Ask(prompt string) (string, error)       { return f.next(prompt) }
func (f *fakeUI) AskSecret(prompt string) (string, error) { return f.next(prompt) }
func (f *fakeUI) Confirm(prompt string) (bool, error) {
	a, err := f.next(prompt)
	return strings.HasPrefix(strings.ToLower(a), "y"), err
}

func newTestAgent(t *testing.T) (*Agent, *HostStore, *Memory) {
	t.Helper()
	dir := t.TempDir()
	st, err := NewHostStore(filepath.Join(dir, "servers"))
	if err != nil {
		t.Fatal(err)
	}
	mem, err := NewMemory(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	a := &Agent{
		cfg: cfg, store: st, mem: mem, ui: &fakeUI{}, conns: map[string]*Conn{},
		dial: newDialer(cfg),
	}
	a.reset()
	return a, st, mem
}

func TestToolAddServerRejectsBadIP(t *testing.T) {
	a, _, _ := newTestAgent(t)
	out := a.toolAddServer("not an ip!!", "root", "test", 22, "", nil)
	if !strings.Contains(out, "错误") {
		t.Errorf("非法地址应被拒绝: %q", out)
	}
}

func TestToolAddServerSavesAndTests(t *testing.T) {
	a, st, _ := newTestAgent(t)
	ui := a.ui.(*fakeUI)
	ui.answers = []string{"secret-pw", ""} // 密码，密钥路径留空
	out := a.toolAddServer("127.0.0.1", "root", "测试机", 5999, "张三", []string{"test"})
	if !strings.Contains(out, "已保存") {
		t.Fatalf("应保存成功: %q", out)
	}
	// 连接测试会失败（端口不通），但必须如实报告而不是假装成功
	if !strings.Contains(out, "连接测试失败") {
		t.Errorf("连不上时应如实报告: %q", out)
	}
	rec, err := st.Find("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Password != "secret-pw" || rec.Purpose != "测试机" || rec.Owner != "张三" {
		t.Errorf("记录内容错误: %+v", rec)
	}
	if rec.Port != 5999 {
		t.Errorf("端口错误: %d", rec.Port)
	}
	// 重复添加应提示已存在
	out2 := a.toolAddServer("127.0.0.1", "root", "x", 5999, "", nil)
	if !strings.Contains(out2, "已存在") {
		t.Errorf("重复添加应提示: %q", out2)
	}
}

func TestToolListServersAndRemember(t *testing.T) {
	a, st, mem := newTestAgent(t)
	if out := a.toolListServers(); !strings.Contains(out, "没有录入") {
		t.Errorf("空列表提示错误: %q", out)
	}
	st.Save(&HostRecord{ID: "web-01", IP: "10.0.1.11", Username: "root", Purpose: "Nginx"})
	out := a.toolListServers()
	if !strings.Contains(out, "web-01") || !strings.Contains(out, "Nginx") {
		t.Errorf("列表内容错误: %q", out)
	}

	if r := a.toolRemember("web-01 磁盘告警已解除", []string{"web-01"}, []string{"记得清理日志"}); !strings.Contains(r, "已写入") {
		t.Errorf("记忆写入失败: %q", r)
	}
	all, _ := mem.ReadAll()
	if !strings.Contains(all, "磁盘告警已解除") {
		t.Errorf("MEMORY.md 未写入: %s", all)
	}
	rec, _ := st.Find("web-01")
	if rec.LastSummary != "web-01 磁盘告警已解除" {
		t.Errorf("最近结论未同步到服务器文件: %q", rec.LastSummary)
	}
	if len(rec.Notes) != 1 || rec.Notes[0] != "记得清理日志" {
		t.Errorf("备注未同步: %v", rec.Notes)
	}
}

func TestSaveMemoryFromReply(t *testing.T) {
	a, st, mem := newTestAgent(t)
	st.Save(&HostRecord{ID: "db-01", IP: "10.0.2.21", Username: "root", Purpose: "MySQL"})
	reply := "数据库正常。\n<memory>\nsummary: db-01 MySQL 连接数 120，正常\nhost: db-01\ndetail: 无明显异常\n</memory>"
	a.saveMemoryFromReply(reply)
	all, _ := mem.ReadAll()
	if !strings.Contains(all, "连接数 120") {
		t.Errorf("应自动写入记忆: %s", all)
	}
	rec, _ := st.Find("db-01")
	if !strings.Contains(rec.LastSummary, "连接数 120") {
		t.Errorf("应更新机器记忆: %q", rec.LastSummary)
	}
}

func TestLookupDoesNotLeakPasswordToPrompt(t *testing.T) {
	// 系统提示里绝不能出现密码
	a, st, _ := newTestAgent(t)
	st.Save(&HostRecord{ID: "s1", IP: "1.2.3.4", Username: "root", Password: "TOP-SECRET-PW", Purpose: "x"})
	sys := a.buildSystemPrompt()
	if strings.Contains(sys, "TOP-SECRET-PW") {
		t.Error("系统提示泄露了密码！")
	}
}

func TestRunToolNoPolicyBlock(t *testing.T) {
	// 不做任何限制：灾难命令也会被真正执行（这里连不上服务器，所以返回的是连接错误）
	a, st, _ := newTestAgent(t)
	st.Save(&HostRecord{ID: "s2", IP: "10.9.9.9", Username: "root", Purpose: "x"})
	out := a.toolRun(context.Background(), "s2", "rm -rf /etc", "")
	if strings.Contains(out, "被安全策略拒绝") {
		t.Errorf("不应再被策略拦截: %q", out)
	}
	// 应该是连接失败，而不是策略拒绝
	if !strings.Contains(out, "连接") && !strings.Contains(out, "错误") {
		t.Errorf("应返回连接错误: %q", out)
	}
}
