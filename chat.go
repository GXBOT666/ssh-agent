package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
// 终端对话界面
// ============================================================================

// terminalUI 用标准输入输出实现 UI。
type terminalUI struct {
	in *bufio.Reader
}

func newTerminalUI() *terminalUI {
	return &terminalUI{in: bufio.NewReader(os.Stdin)}
}

func (t *terminalUI) Println(a ...any)          { fmt.Println(a...) }
func (t *terminalUI) Printf(f string, a ...any) { fmt.Printf(f, a...) }

// Ask 读取一行普通输入。终端下由 readLine 提供行编辑（退格/方向键/Home 等），
// 非终端（管道）时走同一个 bufio，保证不会有两个读取者抢输入。
func (t *terminalUI) Ask(prompt string) (string, error) {
	if isTerminal() {
		line, err := readLine(os.Stdin, prompt)
		if errors.Is(err, ErrCanceled) {
			return "", ErrCanceled
		}
		if errors.Is(err, errEOF) {
			return "", errEOF
		}
		return line, err
	}
	return t.askBuffered(prompt)
}

// askBuffered 是管道/重定向/测试时的行读取。
func (t *terminalUI) askBuffered(prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := t.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		return "", errEOF
	}
	return line, nil
}

// AskSecret 读取密码：终端下不回显（逐字符读取，避免与 bufio 抢输入）。
func (t *terminalUI) AskSecret(prompt string) (string, error) {
	if !isTerminal() {
		return t.askBuffered(prompt)
	}
	return readSecretLine(os.Stdin, prompt)
}

// Confirm 是/否确认，默认否。
func (t *terminalUI) Confirm(prompt string) (bool, error) {
	// 说明文字可能有多行（例如风险命令全文），先整段打印出来；
	// 然后把输入框单独放在最后一行，保证用户看得到、也删得动自己输入的内容。
	if strings.Contains(prompt, "\n") {
		fmt.Println(prompt)
		prompt = "确认执行"
	}
	ans, err := t.Ask(prompt + " [y/N] ")
	if err != nil {
		if errors.Is(err, ErrCanceled) || errors.Is(err, errEOF) {
			return false, nil // 取消/结束都按"否"处理，避免误执行
		}
		return false, err
	}
	ans = strings.ToLower(ans)
	return ans == "y" || ans == "yes" || ans == "是", nil
}

// ============================================================================
// chat 命令
// ============================================================================

const chatHelp = `可用指令（对话中随时输入）：
  /help              显示本帮助
  /servers           列出已录入的服务器
  /add               录入一台新服务器（交互式，密码不回显）
  /del <ID>          删除一台服务器
  /edit <ID>         修改服务器的用途/负责人/备注
  /connect <ID>      单独测试连接某台服务器
  /model             换模型或重新填密钥
  /memory            查看最近的记忆条目
  /remember <内容>   手动写一条记忆
  /reset             清空当前对话上下文（记忆文件保留）
  /quit 或 /exit     退出

直接输入中文即可与 AI 对话，例如：
  "看看 web-01 磁盘还剩多少"
  "帮我把 10.0.1.11 加进来，是生产环境跑 Nginx 的"
  "db-01 上 MySQL 正常吗"
`

func cmdChat(args []string) int {
	cf := &commonFlags{}
	fs := newFlagSet("chat", cf)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cf.apply()
	// 按使用者要求：不做任何限制，AI 想执行什么就执行什么

	ui := newTerminalUI()

	// 从本地设置读取模型；没有就现场引导。不需要任何配置文件。
	st, err := LoadSettings()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if !st.Complete() || cf.resetSettings {
		if st, err = setupModelWizard(ui); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	cfg := st.runtimeConfig()

	store, err := NewHostStore(filepath.Join(cfg.DataDir, "servers"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	mem, err := NewMemory(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	agent, err := NewAgent(cfg, store, mem, ui)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer agent.Close()

	// 启动自检：接口不通就别进对话，直接说清楚怎么改
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 30*time.Second)
	pingErr := agent.llm.PingModel(pingCtx)
	cancelPing()
	if pingErr != nil {
		fmt.Fprintf(os.Stderr, "%v\n", pingErr)
		fmt.Fprintf(os.Stderr, "运行 %s --reset 重新配置\\n", "Ai")
		return 2
	}

	printChatBanner(cfg, store, mem)

	ctx, cancel := signalContext()
	defer cancel()

	// 允许一次性提问：sshex chat "看看 web-01 的磁盘"
	if rest := fs.Args(); len(rest) > 0 {
		askOnce(ctx, agent, ui, strings.Join(rest, " "))
		return 0
	}

	for {
		input, err := ui.Ask(color("\n你 > ", 36))
		if err != nil {
			if errors.Is(err, errEOF) || errors.Is(err, io.EOF) {
				fmt.Println()
				return 0 // Ctrl-D 或输入结束
			}
			if errors.Is(err, ErrCanceled) {
				fmt.Println(dim("（已取消，Ctrl-D 退出）"))
				continue
			}
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}
		if strings.HasPrefix(input, "/") {
			quit := handleSlash(ctx, agent, store, mem, ui, input)
			if quit {
				return 0
			}
			continue
		}
		askOnce(ctx, agent, ui, input)
	}
}

// setupModelWizard 首次运行时询问模型接口信息并写回配置。
func setupModelWizard(ui UI) (*Settings, error) {
	presets := []struct{ name, base string }{
		{"DeepSeek", "https://api.deepseek.com"},
		{"OpenAI", "https://api.openai.com/v1"},
		{"通义千问", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"Kimi", "https://api.moonshot.cn/v1"},
		{"本地 Ollama", "http://127.0.0.1:11434/v1"},
	}
	for i, p := range presets {
		ui.Printf("%d %s\n", i+1, p.name)
	}
	choice, _ := ui.Ask("> ")
	idx := 1
	if n := atoiLoose(choice); n >= 1 && n <= len(presets) {
		idx = n
	}
	pre := presets[idx-1]

	base, _ := ui.Ask(pre.base + " > ")
	if strings.TrimSpace(base) == "" {
		base = pre.base
	}
	base = strings.TrimSpace(base)

	key, err := ui.AskSecret("key > ")
	if err != nil {
		return nil, fmt.Errorf("读取 key 失败: %w", err)
	}
	if strings.TrimSpace(key) == "" && !isLocalEndpoint(base) {
		return nil, errors.New("没有输入 key")
	}

	// 向接口查询实际可用的模型
	var model string
	models, lerr := ListModels(context.Background(), base, strings.TrimSpace(key))
	if lerr == nil && len(models) > 0 {
		for i, m := range models {
			ui.Printf("%d %s\n", i+1, m)
		}
		pick, _ := ui.Ask("> ")
		n := atoiLoose(pick)
		if n >= 1 && n <= len(models) {
			model = models[n-1]
		} else {
			model = strings.TrimSpace(pick)
		}
	}
	if model == "" {
		if lerr != nil {
			ui.Println(dim("查不到模型列表，手动输入模型名"))
		}
		model, _ = ui.Ask("model > ")
		model = strings.TrimSpace(model)
		if model == "" {
			return nil, errors.New("没有选择模型")
		}
	}

	st := &Settings{Provider: pre.name, BaseURL: base, APIKey: strings.TrimSpace(key), Model: model}
	// 保存前先试一次，key 或模型名不对就不落盘，避免把坏设置存下来
	if client, err := NewLLMClient(st.runtimeConfig().Model); err == nil {
		if err := client.PingModel(context.Background()); err != nil {
			return nil, fmt.Errorf("%v", err)
		}
	}
	if err := st.Save(); err != nil {
		return nil, fmt.Errorf("保存设置失败: %w", err)
	}
	return st, nil
}

func printChatBanner(cfg *Config, store *HostStore, mem *Memory) {
	hosts, _ := store.List()
	fmt.Printf("%s  %s  %d 台  记忆 %d 条 (%s)\n",
		color("sshex", 1), dim(cfg.Model.Model), len(hosts), mem.Count(), shortenPath(mem.MemoryMD))
	warnLegacyHome()
}

// warnLegacyHome 提示旧版本遗留在用户主目录的数据（只提示，不自动删除）。
func warnLegacyHome() {
	old := legacyHome()
	if old == "" {
		return
	}
	if _, err := os.Stat(old); err != nil {
		return
	}
	if samePath(old, dataHome()) {
		return
	}
	fmt.Println(dim("旧版数据还在 " + shortenPath(old) + "，已不再使用，可删除: rm -rf " + old))
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return ra == rb
}

func askOnce(ctx context.Context, agent *Agent, ui UI, input string) {
	stop := spinner()
	reply, err := agent.Ask(ctx, input)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return
	}
	fmt.Printf("%s %s\n", color("AI >", 32), reply)
}

// spinner 在等待时原地刷新，结束后清除该行。非终端下不输出。
func spinner() func() {
	if !isOutputTerminal() {
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		frames := []string{"·", "✢", "✳", "✶", "✻", "✽"}
		for i := 0; ; i++ {
			select {
			case <-done:
				fmt.Print("\r\033[K")
				return
			case <-time.After(90 * time.Millisecond):
				fmt.Printf("\r%s", dim(frames[i%len(frames)]))
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// handleSlash 处理斜杠指令，返回 true 表示退出。
func handleSlash(ctx context.Context, a *Agent, store *HostStore, mem *Memory, ui UI, input string) bool {
	fields := strings.Fields(input)
	cmd := strings.ToLower(fields[0])
	arg := strings.TrimSpace(strings.TrimPrefix(input, fields[0]))

	switch cmd {
	case "/quit", "/exit", "/q":
		fmt.Println("再见，记忆已保存。")
		return true

	case "/help", "/?":
		fmt.Print(chatHelp)

	case "/model":
		st, err := setupModelWizard(ui)
		if err != nil {
			ui.Println(err)
			return false
		}
		cfg := st.runtimeConfig()
		llm, err := NewLLMClient(cfg.Model)
		if err != nil {
			ui.Println(err)
			return false
		}
		pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
		err = llm.PingModel(pctx)
		pcancel()
		if err != nil {
			ui.Println(err)
			return false
		}
		a.cfg = cfg
		a.llm = llm
		a.Reset()
		ui.Println(st.Model)

	case "/reset":
		a.Reset()
		ui.Println("已清空对话上下文（服务器与记忆保持不变）。")

	case "/servers", "/list", "/ls":
		printServerTable(store, ui)

	case "/add":
		interactiveAdd(store, ui)

	case "/del", "/delete":
		if arg == "" {
			ui.Println("用法: /del <服务器ID>")
			return false
		}
		rec, err := store.Find(arg)
		if err != nil {
			ui.Println("错误:", err)
			return false
		}
		ok, _ := ui.Confirm(fmt.Sprintf("确定删除服务器 %s（%s，%s）？文件也会被删除", rec.ID, rec.Addr(), orNone(rec.Purpose)))
		if !ok {
			ui.Println("已取消。")
			return false
		}
		if err := store.Delete(rec.ID); err != nil {
			ui.Println("删除失败:", err)
			return false
		}
		a.Reset()
		ui.Println("已删除:", rec.ID)

	case "/edit":
		if arg == "" {
			ui.Println("用法: /edit <服务器ID>")
			return false
		}
		interactiveEdit(store, ui, arg)

	case "/connect":
		if arg == "" {
			ui.Println("用法: /connect <服务器ID>")
			return false
		}
		rec, err := store.Find(arg)
		if err != nil {
			ui.Println("错误:", err)
			return false
		}
		conn, err := a.connectionFor(ctx, rec)
		if err != nil {
			ui.Printf("连接 %s 失败: %v\n", rec.ID, err)
			return false
		}
		r := conn.Run(ctx, "hostname; uname -sr; whoami", 20*time.Second)
		ui.Printf("连接 %s (%s) 成功，主机指纹 %s\n%s", rec.ID, conn.Username+"@"+shortHost(rec.IP, rec.Port), conn.HostKey.SHA256, r.Stdout)

	case "/memory", "/mem":
		ui.Printf("%s:%d 条\n", shortenPath(mem.MemoryMD), mem.Count())
		ui.Printf("%s\n", shortenPath(mem.History))
		if txt, err := mem.ReadAll(); err == nil {
			for _, sec := range lastSections(txt, 5) {
				ui.Println(cleanForTerminal(sec) + "\n")
			}
		}

	case "/remember":
		if arg == "" {
			ui.Println("用法: /remember <要记住的内容>")
			return false
		}
		if err := mem.Append(MemoryEntry{Time: time.Now(), Summary: arg}); err != nil {
			ui.Println("写入失败:", err)
			return false
		}
		ui.Println("已写入记忆。")

	default:
		ui.Printf("未知指令 %s，输入 /help 查看可用指令\n", cmd)
	}
	return false
}

// printServerTable 打印服务器清单。
func printServerTable(store *HostStore, ui UI) {
	hosts, err := store.List()
	if err != nil {
		ui.Println("读取失败:", err)
		return
	}
	if len(hosts) == 0 {
		ui.Println("还没有录入服务器。输入 /add 录入，或直接告诉 AI「帮我添加一台服务器」。")
		return
	}
	ui.Printf("\n%-16s %-22s %-10s %-26s %s\n", "ID", "地址", "账号", "用途", "最近结论")
	for _, h := range hosts {
		ui.Printf("%-16s %-22s %-10s %-26s %s\n",
			h.ID, shortHost(h.IP, portOrDefault(h.Port)), truncate(h.Username, 10),
			truncate(orNone(h.Purpose), 26), truncate(firstLine(h.LastSummary), 40))
	}
	ui.Printf("\n共 %d 台，文件位于 %s\n", len(hosts), store.Dir())
}

// interactiveAdd 交互式录入服务器。密码直接读终端，不经过 AI。
func interactiveAdd(store *HostStore, ui UI) {
	ip, err := ui.Ask("ip > ")
	if err != nil || strings.TrimSpace(ip) == "" {
		ui.Println("已取消。")
		return
	}
	ip = strings.TrimSpace(ip)
	portStr, _ := ui.Ask("port > ")
	port := atoiLoose(portStr)
	user, _ := ui.Ask("user > ")
	user = strings.TrimSpace(user)
	if user == "" {
		user = "root"
	}
	purpose, _ := ui.Ask("用途 > ")
	owner, _ := ui.Ask("负责人 > ")
	tagsStr, _ := ui.Ask("标签 > ")

	pw, err := ui.AskSecret("pass > ")
	if err != nil {
		ui.Println("读取密码失败:", err)
		return
	}
	rec := &HostRecord{
		ID:       ip,
		IP:       ip,
		Port:     portOrDefault(port),
		Username: user,
		Password: pw,
		Purpose:  strings.TrimSpace(purpose),
		Owner:    strings.TrimSpace(owner),
		Tags:     splitList(tagsStr),
	}
	if pw == "" {
		keyFile, _ := ui.Ask("keyfile > ")
		rec.KeyFile = expandHome(strings.TrimSpace(keyFile))
	}
	if err := store.Save(rec); err != nil {
		ui.Println("保存失败:", err)
		return
	}
	ui.Println(store.fileFor(rec.ID))
}

// interactiveEdit 修改已有服务器的用途/负责人/备注。
func interactiveEdit(store *HostStore, ui UI, id string) {
	rec, err := store.Find(id)
	if err != nil {
		ui.Println("错误:", err)
		return
	}
	ui.Printf("当前用途: %s\n", orNone(rec.Purpose))
	if v, _ := ui.Ask("新用途（回车保留）: "); strings.TrimSpace(v) != "" {
		rec.Purpose = strings.TrimSpace(v)
	}
	if v, _ := ui.Ask("新负责人（回车保留）: "); strings.TrimSpace(v) != "" {
		rec.Owner = strings.TrimSpace(v)
	}
	if v, _ := ui.Ask("追加备注（回车跳过）: "); strings.TrimSpace(v) != "" {
		store.MergeNotes(rec, []string{strings.TrimSpace(v)})
	}
	if err := store.Save(rec); err != nil {
		ui.Println("保存失败:", err)
		return
	}
	ui.Println("已更新。")
}

// cleanForTerminal 去掉 Markdown 里对终端没用的折叠标签。
func cleanForTerminal(sec string) string {
	sec = strings.ReplaceAll(sec, "<details><summary>执行记录</summary>", "执行记录")
	sec = strings.ReplaceAll(sec, "</details>", "")
	return strings.TrimSpace(sec)
}

// lastSections 取 MEMORY.md 最后 n 个小节。
func lastSections(txt string, n int) []string {
	var secs []string
	var cur []string
	for _, line := range strings.Split(txt, "\n") {
		if strings.HasPrefix(line, "## ") {
			if len(cur) > 0 {
				secs = append(secs, strings.Join(cur, "\n"))
			}
			cur = []string{line}
			continue
		}
		if len(cur) > 0 {
			cur = append(cur, line)
		}
	}
	if len(cur) > 0 {
		secs = append(secs, strings.Join(cur, "\n"))
	}
	if len(secs) > n {
		secs = secs[len(secs)-n:]
	}
	return secs
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '，' || r == ' ' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// newBufReader 创建行读取器（测试与管道模式共用）。
func newBufReader(r *os.File) *bufio.Reader { return bufio.NewReader(r) }
