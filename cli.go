package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// ============================================================================
// 公共参数
// ============================================================================

type commonFlags struct {
	config        string
	dryRun        bool
	yes           bool
	verbose       bool
	resetSettings bool
}

// newFlagSet 创建一个带公共参数的 FlagSet。
func newFlagSet(name string, cf *commonFlags) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cf.config, "c", "", "配置文件路径")
	fs.StringVar(&cf.config, "config", "", "配置文件路径")
	fs.BoolVar(&cf.dryRun, "dry-run", false, "只展示将要执行的工作，不连接主机")
	fs.BoolVar(&cf.yes, "y", false, "自动确认风险操作")
	fs.BoolVar(&cf.yes, "yes", false, "自动确认风险操作")
	fs.BoolVar(&cf.verbose, "v", false, "输出调试信息")
	fs.BoolVar(&cf.verbose, "verbose", false, "输出调试信息")
	fs.BoolVar(&cf.resetSettings, "reset", false, "重新配置模型接口")
	return fs
}

func (cf *commonFlags) apply() {
	verbose = cf.verbose
	dryRun = cf.dryRun
}

func (cf *commonFlags) load() (*Config, int) {
	cfg, err := loadWithFallback(cf.config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, 2
	}
	return cfg, 0
}

// loadWithFallback：找不到默认配置文件时，如果当前目录有 config.example.json
// 也提示一下，避免新手一脸茫然。
func loadWithFallback(explicit string) (*Config, error) {
	if explicit == "" {
		if _, err := findConfig(""); err != nil {
			return nil, errors.New("找不到配置文件\n\n直接运行 ./Ai 即可，不需要配置文件——模型接口、密钥、服务器都在对话里录入")
		}
	}
	path, err := findConfig(explicit)
	if err != nil {
		return nil, err
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "使用配置: %s\n", path)
	}
	return LoadConfig(path)
}

// multiString 支持 -H a -H b 以及 -H a,b。
type multiString []string

func (m *multiString) String() string { return strings.Join(*m, ",") }
func (m *multiString) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*m = append(*m, part)
		}
	}
	return nil
}

// signalContext 在 Ctrl-C 时取消上下文，让正在运行的任务尽量收尾。
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// ============================================================================
// run / report
// ============================================================================

func cmdRun(args []string, forceReport bool) int {
	cf := &commonFlags{}
	fs := newFlagSet("run", cf)
	var groups, hosts, taskTags multiString
	var taskNames multiString
	var timeout time.Duration
	var reportDir string
	var asJSON bool
	fs.Var(&groups, "g", "只执行指定分组")
	fs.Var(&groups, "group", "只执行指定分组")
	fs.Var(&hosts, "H", "只执行指定主机")
	fs.Var(&hosts, "host", "只执行指定主机")
	fs.Var(&taskTags, "tags", "只执行带这些标签的任务")
	fs.Var(&taskNames, "task", "只执行指定任务名")
	fs.StringVar(&reportDir, "report", "", "生成 Markdown 报告的目录")
	fs.DurationVar(&timeout, "timeout", 0, "覆盖所有任务的超时，例如 10m")
	fs.BoolVar(&asJSON, "json", false, "以 JSON 输出结果")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cf.apply()
	jsonOut = asJSON
	if forceReport && reportDir == "" {
		reportDir = "reports"
	}

	cfg, code := cf.load()
	if code != 0 {
		return code
	}

	opt := RunOptions{
		Groups: groups, Hosts: hosts, TaskTags: taskTags, OnlyTaskNames: taskNames,
		TimeoutOverride: timeout,
	}
	if groups == nil && hosts == nil {
		opt.Groups, opt.Hosts = nil, nil
	}

	plan, err := cfg.BuildPlan(opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 2
	}
	if len(plan.Tasks) == 0 {
		fmt.Fprintln(os.Stderr, "提示: 没有可执行的任务（请检查 tasks 的 enabled 字段或 --tags 筛选）")
		return 2
	}

	ctx, cancel := signalContext()
	defer cancel()

	sum := Execute(ctx, cfg, plan, opt)

	if asJSON {
		out := summaryJSON(sum)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
	} else {
		PrintRunSummary(sum)
	}

	if reportDir != "" {
		path, err := WriteMarkdownReport(sum, cfg.resolvePath(reportDir), false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "生成报告失败:", err)
			return 1
		}
		if asJSON {
			fmt.Fprintf(os.Stderr, "报告已生成: %s\n", path)
		} else {
			fmt.Printf("\n报告已生成: %s\n", path)
		}
	}

	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "运行被中断")
		return 1
	}
	if sum.HasFailures() {
		return 1
	}
	return 0
}

// runJSON 是 --json 输出的结构。
type runJSON struct {
	Name     string           `json:"name"`
	Config   string           `json:"config"`
	Start    string           `json:"start"`
	Duration string           `json:"duration"`
	Totals   map[string]int   `json:"totals"`
	Hosts    []hostResultJSON `json:"hosts"`
}

type hostResultJSON struct {
	ID       string           `json:"id"`
	Addr     string           `json:"addr"`
	Role     string           `json:"role,omitempty"`
	Identity string           `json:"intro,omitempty"`
	Error    string           `json:"error,omitempty"`
	Tasks    []taskResultJSON `json:"tasks"`
}

type taskResultJSON struct {
	Name     string             `json:"name"`
	Status   string             `json:"status"`
	ExitCode int                `json:"exit_code"`
	Duration string             `json:"duration"`
	Detail   string             `json:"detail,omitempty"`
	Error    string             `json:"error,omitempty"`
	Stdout   string             `json:"stdout,omitempty"`
	Stderr   string             `json:"stderr,omitempty"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
}

func summaryJSON(sum *RunSummary) runJSON {
	out := runJSON{
		Name: sum.Config.Name, Config: sum.Config.path,
		Start: sum.Start.Format(time.RFC3339), Duration: sum.End.Sub(sum.Start).String(),
		Totals: map[string]int{},
	}
	for k, v := range sum.Totals() {
		out.Totals[string(k)] = v
	}
	for _, h := range sum.Hosts {
		hj := hostResultJSON{
			ID: h.Server.ID, Addr: shortHost(h.Server.Host, h.Server.Port),
			Role: h.Identity.Role, Identity: h.Identity.Description,
		}
		if h.Err != nil {
			hj.Error = h.Err.Error()
		}
		for _, r := range h.Results {
			tj := taskResultJSON{
				Name: r.Task.Name, Status: string(r.Status), ExitCode: r.ExitCode,
				Duration: r.Duration.String(), Detail: r.Detail, Stdout: r.Stdout, Stderr: r.Stderr,
				Metrics: r.Metrics,
			}
			if r.Err != nil {
				tj.Error = r.Err.Error()
			}
			hj.Tasks = append(hj.Tasks, tj)
		}
		out.Hosts = append(out.Hosts, hj)
	}
	return out
}

// ============================================================================
// exec：单主机临时命令
// ============================================================================

func cmdExec(args []string) int {
	cf := &commonFlags{}
	fs := newFlagSet("exec", cf)
	var host, cmd, timeoutS string
	fs.StringVar(&host, "H", "", "目标主机 id/name")
	fs.StringVar(&host, "host", "", "目标主机 id/name")
	fs.StringVar(&cmd, "cmd", "", "要执行的命令")
	fs.StringVar(&timeoutS, "timeout", "60s", "超时")
	fs.BoolVar(&cf.yes, "no-confirm", false, "与 -y 相同：风险命令不弹确认")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cf.apply()
	// 位置参数也作为命令：sshex exec -H web-01 'df -h'
	if cmd == "" && fs.NArg() > 0 {
		cmd = strings.Join(fs.Args(), " ")
	}
	if host == "" || cmd == "" {
		fmt.Fprintln(os.Stderr, "用法: sshex exec -H <host> --cmd '<命令>' [--timeout 60s]")
		return 2
	}
	timeout, err := time.ParseDuration(timeoutS)
	if err != nil {
		fmt.Fprintln(os.Stderr, "超时格式错误:", err)
		return 2
	}

	cfg, code := cf.load()
	if code != 0 {
		return code
	}
	servers, err := cfg.selectServers(nil, []string{host})
	if err != nil || len(servers) == 0 {
		if err == nil {
			err = errors.New("主机不存在")
		}
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 2
	}
	srv := servers[0]

	fmt.Printf("主机 %s（%s）\n", srv.ID, cfg.EffectiveIdentity(srv).Description)
	fmt.Printf("用户 %s@%s\n", firstNonEmpty(srv.Username, "(由密钥决定)"), shortHost(srv.Host, srv.Port))
	fmt.Printf("命令: %s\n\n", cmd)

	ctx, cancel := signalContext()
	defer cancel()
	dial := newDialer(cfg)
	defer dial.Close()
	conn, err := dial.Connect(ctx, srv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接失败:", err)
		return 1
	}
	defer conn.Close()
	fmt.Printf("已登录，主机指纹 %s\n\n", conn.HostKey.SHA256)

	r := conn.Run(ctx, cmd, timeout)
	if r.Stdout != "" {
		fmt.Print(r.Stdout)
	}
	if r.Stderr != "" {
		fmt.Fprint(os.Stderr, r.Stderr)
	}
	if r.Err != nil {
		fmt.Fprintln(os.Stderr, "执行出错:", r.Err)
		return 1
	}
	fmt.Printf("\n[退出码 %d，耗时 %s]\n", r.ExitCode, r.Duration.Round(time.Millisecond))
	if r.ExitCode != 0 {
		return 1
	}
	return 0
}

// ============================================================================
// check：体检
// ============================================================================

func cmdCheck(args []string) int {
	cf := &commonFlags{}
	fs := newFlagSet("check", cf)
	var noLogin bool
	var groups, hosts multiString
	fs.BoolVar(&noLogin, "no-login", false, "只检查配置，不尝试登录")
	fs.Var(&groups, "g", "只检查指定分组")
	fs.Var(&groups, "group", "只检查指定分组")
	fs.Var(&hosts, "H", "只检查指定主机")
	fs.Var(&hosts, "host", "只检查指定主机")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cf.apply()

	path, err := findConfig(cf.config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Printf("配置文件: %s\n", path)

	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取失败:", err)
		return 2
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		fmt.Printf("  ✘ JSON 语法错误: %v\n", err)
		return 2
	}
	fmt.Println("  ✔ JSON 语法正确")

	cfg, err := LoadConfig(path)
	if err != nil {
		fmt.Printf("  ✘ %v\n", err)
		return 2
	}
	fmt.Printf("  ✔ 配置结构校验通过（%d 台服务器，%d 条密钥，%d 项工作）\n", len(cfg.Servers), len(cfg.Keys), len(cfg.Tasks))
	for _, w := range cfg.Warnings() {
		fmt.Printf("  ⚠ %s\n", w)
	}

	// 密钥
	fmt.Println("\n密钥检查:")
	cache := newLoadingCache()
	keyOK := true
	for i := range cfg.Keys {
		k := &cfg.Keys[i]
		t, _ := k.resolveType()
		line := fmt.Sprintf("  - %-14s %-12s", k.Name, t)
		switch t {
		case "private_key":
			if _, err := cache.signer(k); err != nil {
				line += " ✘ " + err.Error()
				keyOK = false
			} else {
				if s, err := cache.signer(k); err == nil {
					line += " ✔ 私钥可解析 " + sshFingerprint(s)
				}
			}
		case "password":
			if ref := undefinedEnvRef(k.Value); ref != "" {
				line += " ⚠ 环境变量 " + ref + " 未设置（登录时会失败）"
			} else if k.Value == "" {
				line += " ✘ 未设置密码"
				keyOK = false
			} else {
				line += " ✔ 已配置（" + Mask(k.Value) + "）"
			}
		case "agent":
			line += " ✔ agent " + k.Value
		case "none":
			line += " ✔ 免认证"
		}
		fmt.Println(line)
	}
	if len(cfg.Keys) == 0 {
		fmt.Println("  （无）")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exit := 0

	// 模型接口检查（AI 助手的核心依赖）
	if cfg.Model.BaseURL != "" || cfg.Model.Model != "" {
		fmt.Println("\n模型检查:")
		fmt.Printf("  - %-10s %s / %s\n", orNone(cfg.Model.Name), cfg.Model.Model, cfg.Model.BaseURL)
		if client, err := NewLLMClient(cfg.Model); err != nil {
			fmt.Printf("  ✘ %v\n", err)
			exit = 1
		} else {
			pctx, pcancel := context.WithTimeout(ctx, 40*time.Second)
			err := client.PingModel(pctx)
			pcancel()
			if err != nil {
				fmt.Printf("  ✘ 接口不可用: %v\n", err)
				exit = 1
			} else {
				fmt.Printf("  ✔ 接口可用（已发送测试请求）\n")
			}
		}
	}

	fmt.Println("\n主机检查:")
	selected, serr := cfg.selectServers(groups, hosts)
	if serr != nil {
		fmt.Fprintln(os.Stderr, "错误:", serr)
		return 2
	}
	picked := map[string]bool{}
	for _, s := range selected {
		picked[s.ID] = true
	}
	dial := newDialer(cfg)
	defer dial.Close()

	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		id := cfg.EffectiveIdentity(s)
		label := fmt.Sprintf("  - %-16s %-22s %-10s", s.ID, shortHost(s.Host, s.Port), id.Role)
		if !truthy(s.Enabled) {
			fmt.Println(label + " ⏭  已禁用")
			continue
		}
		if (groups != nil || hosts != nil) && !picked[s.ID] {
			continue
		}
		if noLogin {
			fmt.Println(label + " ✔ 配置有效")
			continue
		}
		cctx, ccancel := context.WithTimeout(ctx, dial.dialTimeout(s))
		conn, err := dial.Connect(cctx, s)
		ccancel()
		if err != nil {
			fmt.Println(label + " ✘ " + err.Error())
			exit = 1
			continue
		}
		msg := " ✔ 登录成功"
		if conn.HostKey.SHA256 != "" {
			msg += " 指纹 " + conn.HostKey.SHA256
		}
		// 顺手确认一下 shell 能用
		r := conn.Run(ctx, "echo sshex-ok", 10*time.Second)
		if r.Err != nil || !strings.Contains(r.Stdout, "sshex-ok") {
			msg += color("  ⚠ 命令执行异常（检查 bash 是否安装）", 33)
		}
		fmt.Println(label + msg)
		conn.Close()
	}

	if !keyOK {
		exit = 1
	}
	if exit == 0 {
		fmt.Println("\n体检通过。")
	} else {
		fmt.Println("\n体检发现问题，见上方 ✘ 标记。")
	}
	return exit
}

// ============================================================================
// inv / keys / tasks / hosts
// ============================================================================

func cmdInv(args []string) int {
	cf := &commonFlags{}
	fs := newFlagSet("inv", cf)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cf.apply()
	cfg, code := cf.load()
	if code != 0 {
		return code
	}

	fmt.Printf("%s\n", color(cfg.Name, 1))
	if cfg.Description != "" {
		fmt.Printf("%s\n", cfg.Description)
	}
	for _, w := range cfg.Warnings() {
		fmt.Printf("警告: %s\n", w)
	}
	d := cfg.Identities.Default
	fmt.Printf("\n默认身份: 角色=%s 负责人=%s\n", d.Role, firstNonEmpty(d.Owner, "-"))
	if d.Description != "" {
		fmt.Printf("         %s\n", d.Description)
	}

	fmt.Printf("\n%s\n", color("密钥:", 1))
	PrintKeys(cfg)
	fmt.Printf("\n%s\n", color("服务器身份介绍:", 1))
	for i := range cfg.Servers {
		printIdentity(cfg, &cfg.Servers[i])
	}
	fmt.Printf("\n%s\n", color("工作内容:", 1))
	PrintTasks(cfg)
	return 0
}

func cmdKeys(args []string) int {
	cf := &commonFlags{}
	fs := newFlagSet("keys", cf)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cf.apply()
	cfg, code := cf.load()
	if code != 0 {
		return code
	}
	PrintKeys(cfg)
	return 0
}

// PrintKeys 打印密钥清单（绝不输出私钥内容与明文密码）。
func PrintKeys(cfg *Config) {
	cache := newLoadingCache()
	for i := range cfg.Keys {
		k := &cfg.Keys[i]
		t, _ := k.resolveType()
		fmt.Printf("  %s  (%s)\n", color(k.Name, 1), t)
		if k.Description != "" {
			fmt.Printf("      说明: %s\n", k.Description)
		}
		if k.Username != "" {
			fmt.Printf("      用户名: %s\n", k.Username)
		}
		switch t {
		case "private_key":
			if k.Path != "" {
				fmt.Printf("      私钥: %s\n", k.Path)
			} else {
				fmt.Printf("      私钥: 内嵌在配置中（%d 字节）\n", len(k.Value))
			}
			if k.Passphrase != "" {
				fmt.Printf("      口令: %s\n", Mask(k.Passphrase))
			}
			if s, err := cache.signer(k); err == nil {
				fmt.Printf("      公钥指纹: %s\n", sshFingerprint(s))
			} else {
				fmt.Printf("      %s %v\n", color("无法读取:", 31), err)
			}
		case "password":
			if ref := undefinedEnvRef(k.Value); ref != "" {
				fmt.Printf("      密码: 未设置（环境变量 %s 未导出）\n", ref)
			} else {
				fmt.Printf("      密码: %s\n", Mask(k.Value))
			}
		case "agent":
			fmt.Printf("      agent: %s\n", k.Value)
		}
		// 被哪些主机引用
		var used []string
		for _, s := range cfg.Servers {
			if s.Key == k.Name || (s.Jump != nil && s.Jump.Key == k.Name) {
				used = append(used, s.ID)
			}
		}
		if len(used) > 0 {
			fmt.Printf("      用于主机: %s\n", strings.Join(used, ", "))
		}
	}
	if len(cfg.Keys) == 0 {
		fmt.Println("  （无）")
	}
}

func cmdTasks(args []string) int {
	cf := &commonFlags{}
	fs := newFlagSet("tasks", cf)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cf.apply()
	cfg, code := cf.load()
	if code != 0 {
		return code
	}
	PrintTasks(cfg)
	return 0
}

// PrintTasks 打印工作内容清单。
func PrintTasks(cfg *Config) {
	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		state := ""
		if !truthy(t.Enabled) {
			state = color(" [已禁用]", 90)
		}
		fmt.Printf("  %d. %s%s\n", i+1, color(t.Name, 1), state)
		if t.Description != "" {
			fmt.Printf("     目的: %s\n", t.Description)
		}
		fmt.Printf("     类型: %s", t.Type)
		if t.Role != "" {
			fmt.Printf("  角色: %s", t.Role)
		}
		if len(t.Tags) > 0 {
			fmt.Printf("  标签: %s", strings.Join(t.Tags, ","))
		}
		if len(t.Hosts) > 0 {
			fmt.Printf("  主机: %s", strings.Join(t.Hosts, ","))
		}
		if len(t.Groups) > 0 {
			fmt.Printf("  分组: %s", strings.Join(t.Groups, ","))
		}
		fmt.Printf("  超时: %ds\n", t.TimeoutSec)
		switch t.Type {
		case "command":
			cmd := strings.TrimSpace(t.Command)
			multi := strings.Contains(cmd, "\n")
			for j, line := range strings.Split(cmd, "\n") {
				if !multi && j > 0 {
					break
				}
				if multi {
					fmt.Printf("       │ %s\n", line)
				} else {
					fmt.Printf("     命令: %s\n", line)
				}
			}
			if t.Expect != "" {
				fmt.Printf("     期望输出包含: %s\n", t.Expect)
			}
			if t.ExitCode != nil && *t.ExitCode != 0 {
				fmt.Printf("     期望退出码: %d\n", *t.ExitCode)
			}
		case "upload":
			fmt.Printf("     上传: %s -> %s (mode %s)\n", t.Local, t.Remote, t.Mode)
			if t.ExecAfter != "" {
				fmt.Printf("     之后执行: %s\n", t.ExecAfter)
			}
		case "download":
			fmt.Printf("     下载: %s -> %s\n", t.Remote, t.Local)
		}
	}
	if len(cfg.Tasks) == 0 {
		fmt.Println("  （无）")
	}
}

func cmdHosts(args []string) int {
	cf := &commonFlags{}
	fs := newFlagSet("hosts", cf)
	var groups, hosts multiString
	fs.Var(&groups, "g", "按分组筛选")
	fs.Var(&groups, "group", "按分组筛选")
	fs.Var(&hosts, "H", "按主机筛选")
	fs.Var(&hosts, "host", "按主机筛选")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cf.apply()
	cfg, code := cf.load()
	if code != 0 {
		return code
	}
	selected, err := cfg.selectServers(groups, hosts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 2
	}
	picked := map[string]bool{}
	for _, s := range selected {
		picked[s.ID] = true
	}
	fmt.Printf("%-16s %-22s %-10s %-8s %-10s %-6s %s\n", "ID", "地址", "角色", "分组", "负责人", "本次", "介绍")
	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		id := cfg.EffectiveIdentity(s)
		mark := color("✔", 32)
		if !picked[s.ID] {
			mark = color("-", 90)
		}
		if !truthy(s.Enabled) {
			mark = color("✘", 31)
		}
		fmt.Printf("%-16s %-22s %-10s %-8s %-10s %-6s %s\n",
			s.ID, shortHost(s.Host, s.Port), truncate(id.Role, 10), truncate(s.Group, 8),
			truncate(id.Owner, 10), mark, id.Description)
	}
	fmt.Printf("\n共 %d 台，本次选中 %d 台（✘=已禁用，-=未选中）\n", len(cfg.Servers), len(selected))
	return 0
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
