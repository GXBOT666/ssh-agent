package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 运行编排
// ============================================================================

// RunOptions 是 run/report 的筛选条件。
type RunOptions struct {
	Groups          []string
	Hosts           []string
	TaskTags        []string
	OnlyTaskNames   []string
	TimeoutOverride time.Duration
}

// HostPlan 是一台主机本次要做的事。
type HostPlan struct {
	Server  *Server
	Tasks   []*Task
	Skipped []string // 被跳过任务及原因
}

// Plan 汇总本次运行。
type Plan struct {
	Hosts []*HostPlan
	Tasks []*Task
}

// selectServers 按 group / host 筛选目标机。
func (c *Config) selectServers(groups, hosts []string) ([]*Server, error) {
	var out []*Server
	for i := range c.Servers {
		s := &c.Servers[i]
		if groups != nil && !matchAny(groups, s.Group) {
			continue
		}
		if hosts != nil && !(matchAny(hosts, s.ID) || matchAny(hosts, s.Name)) {
			continue
		}
		if !truthy(s.Enabled) {
			continue
		}
		out = append(out, s)
	}
	if len(hosts) > 0 {
		// 检查给定的主机是否存在且启用了
		var missing []string
		for _, h := range hosts {
			found := false
			for _, s := range c.Servers {
				if strings.EqualFold(s.ID, h) || strings.EqualFold(s.Name, h) {
					found = true
					if !truthy(s.Enabled) {
						return nil, fmt.Errorf("主机 %q 已被禁用", h)
					}
				}
			}
			if !found {
				missing = append(missing, h)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("配置里不存在这些主机: %s", strings.Join(missing, ", "))
		}
	}
	return out, nil
}

// taskAppliesTo 判断任务是否作用于该主机。
func (c *Config) taskAppliesTo(t *Task, s *Server) bool {
	if t.Hosts != nil && !(matchAny(t.Hosts, s.ID) || matchAny(t.Hosts, s.Name)) {
		return false
	}
	if t.Groups != nil && !matchAny(t.Groups, s.Group) {
		return false
	}
	if t.Role != "" {
		id := c.EffectiveIdentity(s)
		if !strings.EqualFold(id.Role, t.Role) {
			return false
		}
	}
	return true
}

// BuildPlan 计算机本次要执行的任务矩阵。
func (c *Config) BuildPlan(opt RunOptions) (*Plan, error) {
	servers, err := c.selectServers(opt.Groups, opt.Hosts)
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, errors.New("没有匹配的主机（检查 -g/-H 以及 enabled 字段）")
	}

	plan := &Plan{}
	seen := map[string]*Task{}
	for i := range c.Tasks {
		t := &c.Tasks[i]
		if !truthy(t.Enabled) {
			continue
		}
		if opt.TaskTags != nil && !intersects(opt.TaskTags, t.Tags) {
			continue
		}
		if opt.OnlyTaskNames != nil && !matchAny(opt.OnlyTaskNames, t.Name) {
			continue
		}
		plan.Tasks = append(plan.Tasks, t)
		seen[t.Name] = t
	}
	if len(plan.Tasks) == 0 {
		// 配置里全被禁用或没有任务也不报错，但 run 会提示
	}

	for _, s := range servers {
		hp := &HostPlan{Server: s}
		for _, t := range plan.Tasks {
			if c.taskAppliesTo(t, s) {
				hp.Tasks = append(hp.Tasks, t)
			} else {
				hp.Skipped = append(hp.Skipped, t.Name)
			}
		}
		plan.Hosts = append(plan.Hosts, hp)
	}
	return plan, nil
}

func intersects(filter []string, values []string) bool {
	for _, f := range filter {
		if matchAny(values, f) {
			return true
		}
	}
	return false
}

// RunSummary 汇总一次运行的结果。
type RunSummary struct {
	Start  time.Time
	End    time.Time
	Config *Config
	Opt    RunOptions
	Hosts  []*HostResult
}

// HostResult 是一台主机的运行结果。
type HostResult struct {
	Server   *Server
	Identity Identity
	Conn     *Conn
	Results  []TaskResult
	Err      error // 连接阶段的错误
	Duration time.Duration
}

func (h *HostResult) Counts() (ok, warn, failed, skipped int) {
	for _, r := range h.Results {
		switch r.Status {
		case StatusOK:
			ok++
		case StatusWarn:
			warn++
		case StatusFailed:
			failed++
		case StatusSkipped:
			skipped++
		}
	}
	return
}

// Execute 按计划并发执行。
func Execute(ctx context.Context, cfg *Config, plan *Plan, opt RunOptions) *RunSummary {
	sum := &RunSummary{Start: time.Now(), Config: cfg, Opt: opt}
	exec := newExecutor(cfg)
	dial := newDialer(cfg)
	defer dial.Close()

	limit := cfg.Policy.MaxParallel
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	results := make([]*HostResult, len(plan.Hosts))
	var mu sync.Mutex

	banner(cfg, plan, opt)

	for i, hp := range plan.Hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, hp *HostPlan) {
			defer wg.Done()
			defer func() { <-sem }()
			hr := runHost(ctx, cfg, exec, dial, hp, opt)
			mu.Lock()
			results[i] = hr
			mu.Unlock()
		}(i, hp)
	}
	wg.Wait()

	sum.Hosts = results
	sum.End = time.Now()
	return sum
}

func banner(cfg *Config, plan *Plan, opt RunOptions) {
	if !verbose && !dryRun {
		return
	}
	fmt.Fprintf(os.Stderr, "==== %s ====\n", cfg.Name)
	if cfg.Description != "" {
		fmt.Fprintf(os.Stderr, "%s\n", cfg.Description)
	}
	fmt.Fprintf(os.Stderr, "目标主机 %d 台，任务 %d 项，并发 %d\n", len(plan.Hosts), len(plan.Tasks), cfg.Policy.MaxParallel)
}

// runHost 单台主机上依次执行任务；失败是否阻断由 policy.fail_fast 决定。
func runHost(ctx context.Context, cfg *Config, exec *executor, dial *dialer, hp *HostPlan, opt RunOptions) *HostResult {
	start := time.Now()
	hr := &HostResult{Server: hp.Server, Identity: cfg.EffectiveIdentity(hp.Server)}
	defer func() { hr.Duration = time.Since(start) }()

	if len(hp.Tasks) == 0 {
		return hr
	}

	if dryRun {
		for _, t := range hp.Tasks {
			hr.Results = append(hr.Results, TaskResult{
				Task: t, Server: hp.Server, Status: StatusSkipped,
				Detail: renderDry(t, hp.Server),
			})
		}
		printHostResult(hr)
		return hr
	}

	conn, err := dial.Connect(ctx, hp.Server)
	if err != nil {
		hr.Err = err
		for _, t := range hp.Tasks {
			hr.Results = append(hr.Results, TaskResult{Task: t, Server: hp.Server, Status: StatusFailed, Err: err})
		}
		printHostResult(hr)
		return hr
	}
	hr.Conn = conn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	hostFailed := false
	for _, t := range hp.Tasks {
		if hostFailed && cfg.Policy.FailFast {
			hr.Results = append(hr.Results, TaskResult{Task: t, Server: hp.Server, Status: StatusSkipped, Detail: "fail_fast：主机已有失败"})
			continue
		}
		if ctx.Err() != nil {
			hr.Results = append(hr.Results, TaskResult{Task: t, Server: hp.Server, Status: StatusSkipped, Detail: "已中断"})
			continue
		}
		res := exec.runTask(ctx, conn, t, opt.TimeoutOverride)
		conn.Touch()
		hr.Results = append(hr.Results, res)
		if res.Status == StatusFailed {
			hostFailed = true
			if !cfg.Policy.ContinueOnErr {
				// 立即停止剩余任务
				for _, rest := range hp.Tasks[len(hr.Results):] {
					hr.Results = append(hr.Results, TaskResult{Task: rest, Server: hp.Server, Status: StatusSkipped, Detail: "continue_on_error=false：前序任务失败"})
				}
				break
			}
		}
	}

	printHostResult(hr)
	return hr
}

// HasFailures 判断是否需要非零退出码。
func (s *RunSummary) HasFailures() bool {
	for _, h := range s.Hosts {
		if h.Err != nil {
			return true
		}
		for _, r := range h.Results {
			if r.Status == StatusFailed {
				return true
			}
		}
	}
	return false
}

// Totals 统计所有任务状态。
func (s *RunSummary) Totals() map[TaskStatus]int {
	m := map[TaskStatus]int{}
	for _, h := range s.Hosts {
		for _, r := range h.Results {
			m[r.Status]++
		}
	}
	return m
}

// ============================================================================
// 输出
// ============================================================================

func color(s string, code int) string {
	if !isOutputTerminal() || os.Getenv("NO_COLOR") != "" {
		return s
	}
	return fmt.Sprintf("\x1b[%dm%s\x1b[0m", code, s)
}

func statusText(st TaskStatus) string {
	switch st {
	case StatusOK:
		return color("[OK]  ", 32)
	case StatusWarn:
		return color("[WARN]", 33)
	case StatusFailed:
		return color("[FAIL]", 31)
	case StatusSkipped:
		return color("[SKIP]", 90)
	}
	return "[" + string(st) + "]"
}

var printMu sync.Mutex

// printHostResult 打印一台主机的执行过程（并行时每台整体输出）。
func printHostResult(hr *HostResult) {
	if jsonOut {
		return
	}
	var b strings.Builder
	printMu.Lock()
	defer printMu.Unlock()

	id := hr.Identity
	idLine := hr.Server.ID
	var meta []string
	if id.Role != "" {
		meta = append(meta, id.Role)
	}
	if hr.Server.Group != "" {
		meta = append(meta, "组:"+hr.Server.Group)
	}
	if id.Owner != "" {
		meta = append(meta, "负责人:"+id.Owner)
	}
	if len(meta) > 0 {
		idLine += " (" + strings.Join(meta, " · ") + ")"
	}
	fmt.Fprintf(&b, "\n%s  %s\n", color("▶", 36), color(idLine, 1))
	if id.Description != "" {
		fmt.Fprintf(&b, "   介绍: %s\n", id.Description)
	}
	addr := shortHost(hr.Server.Host, hr.Server.Port)
	if hr.Conn != nil {
		addr = fmt.Sprintf("%s@%s", hr.Conn.Username, addr)
		if hr.Conn.Transport != "" {
			addr += "（" + hr.Conn.Transport + "）"
		}
	}
	detailLine := "   地址: " + addr
	fmt.Fprintf(&b, "%s\n", detailLine)

	if hr.Err != nil {
		fmt.Fprintf(&b, "   %s 主机不可用: %v\n", statusText(StatusFailed), hr.Err)
	}
	for _, r := range hr.Results {
		line := fmt.Sprintf("   %s %-28s %6s", statusText(r.Status), r.Task.Name, r.Duration.Round(time.Millisecond))
		if r.Detail != "" {
			line += "  " + r.Detail
		}
		b.WriteString(line + "\n")
		for _, a := range r.Alerts {
			fmt.Fprintf(&b, "             %s\n", color("⚠ "+a, 33))
		}
		// 跳过时说明原因（例如被安全策略拦截、需要交互确认）
		if r.Err != nil && r.Status == StatusSkipped && r.Detail == "" {
			fmt.Fprintf(&b, "             %s\n", color("原因: "+firstLine(r.Err.Error()), 33))
		}
		// 连接阶段已整体报错时，不再逐条重复同一个错误
		if r.Err != nil && r.Status != StatusSkipped && hr.Err == nil {
			for _, l := range strings.Split(strings.TrimSpace(r.Err.Error()), "\n") {
				fmt.Fprintf(&b, "             %s\n", color(l, 31))
			}
		}
		if verbose && strings.TrimSpace(r.Stdout) != "" {
			out := strings.TrimSpace(r.Stdout)
			if len(out) > 4000 {
				out = out[:4000] + "\n...(省略)"
			}
			for _, l := range strings.Split(out, "\n") {
				fmt.Fprintf(&b, "             │ %s\n", l)
			}
		}
	}
	if hr.Conn != nil {
		b.WriteString(fmt.Sprintf("   完成，用时 %s\n", hr.Duration.Round(time.Millisecond)))
	}
	os.Stdout.WriteString(b.String())
}

// PrintRunSummary 打印汇总表。
func PrintRunSummary(s *RunSummary) {
	tot := s.Totals()
	fmt.Printf("\n%s\n", color("==== 运行汇总 ====", 1))
	fmt.Printf("配置: %s\n", s.Config.path)
	if s.Config.Description != "" {
		fmt.Printf("       %s\n", s.Config.Description)
	}
	fmt.Printf("耗时: %s   主机: %d   并发: %d\n", s.End.Sub(s.Start).Round(time.Millisecond), len(s.Hosts), s.Config.Policy.MaxParallel)
	fmt.Printf("任务: 成功 %d / 警告 %d / 失败 %d / 跳过 %d\n",
		tot[StatusOK], tot[StatusWarn], tot[StatusFailed], tot[StatusSkipped])

	var failedHosts []string
	for _, h := range s.Hosts {
		ok, warn, failed, skipped := h.Counts()
		mark := color("✔", 32)
		if h.Err != nil || failed > 0 {
			mark = color("✘", 31)
			failedHosts = append(failedHosts, h.Server.ID)
		} else if warn > 0 {
			mark = color("!", 33)
		}
		fmt.Printf("  %s %-16s ok=%d warn=%d fail=%d skip=%d\n", mark, h.Server.ID, ok, warn, failed, skipped)
	}
	if len(failedHosts) > 0 {
		sort.Strings(failedHosts)
		fmt.Printf("\n%s 失败主机: %s\n", color("注意:", 31), strings.Join(failedHosts, ", "))
	}
}

// ============================================================================
// inv / hosts / tasks 等输出命令共用的工具
// ============================================================================

func printIdentity(cfg *Config, s *Server) {
	id := cfg.EffectiveIdentity(s)
	enabled := truthy(s.Enabled)
	state := color("启用", 32)
	if !enabled {
		state = color("禁用", 90)
	}
	fmt.Printf("\n%s  %s\n", color("●", 36), color(s.ID, 1))
	fmt.Printf("   状态: %s   地址: %s\n", state, shortHost(s.Host, s.Port))
	if id.Role != "" {
		fmt.Printf("   角色: %s\n", id.Role)
	}
	if s.Group != "" {
		fmt.Printf("   分组: %s\n", s.Group)
	}
	if id.Owner != "" {
		fmt.Printf("   负责人: %s\n", id.Owner)
	}
	if id.Description != "" {
		fmt.Printf("   介绍: %s\n", id.Description)
	}
	if len(id.Tags) > 0 {
		fmt.Printf("   标签: %s\n", strings.Join(id.Tags, ", "))
	}
	for _, n := range id.Notes {
		fmt.Printf("   备注: %s\n", n)
	}
	if k, ok := findKey(cfg.Keys, s.Key); ok {
		fmt.Printf("   认证: 密钥 %s（用户 %s）\n", k.Name, firstNonEmpty(s.Username, k.Username, "(当前用户)"))
	}
	if s.Jump != nil {
		fmt.Printf("   跳板: %s\n", jumpLabel(s.Jump))
	}
	hk := s.HostKey
	fmt.Printf("   指纹策略: %s", hk.Policy)
	if len(hk.Fingerprints) > 0 {
		var fps []string
		for _, f := range hk.Fingerprints {
			if n, err := parseFingerprint(f); err == nil {
				fps = append(fps, n)
			}
		}
		fmt.Printf(" (%s)", strings.Join(fps, ", "))
	}
	fmt.Println()
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
