package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// WriteMarkdownReport 把一次运行结果写成 Markdown 报告，返回文件路径。
func WriteMarkdownReport(s *RunSummary, dir string, includeHosts bool) (string, error) {
	if dir == "" {
		dir = s.Config.Report.Dir
	}
	if dir == "" {
		dir = filepath.Join(s.Config.dir, "reports")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("sshex-report-%s.md", s.Start.Format("20060102-150405"))
	path := filepath.Join(dir, name)

	var b strings.Builder
	cfg := s.Config
	fmt.Fprintf(&b, "# %s 巡检报告\n\n", cfg.Name)
	if cfg.Description != "" {
		fmt.Fprintf(&b, "> %s\n\n", cfg.Description)
	}
	fmt.Fprintf(&b, "- 生成时间：%s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- 配置文件：`%s`\n", cfg.path)
	fmt.Fprintf(&b, "- 运行耗时：%s\n", s.End.Sub(s.Start).Round(time.Millisecond))
	if len(s.Opt.Groups) > 0 {
		fmt.Fprintf(&b, "- 分组筛选：%s\n", strings.Join(s.Opt.Groups, ", "))
	}
	if len(s.Opt.Hosts) > 0 {
		fmt.Fprintf(&b, "- 主机筛选：%s\n", strings.Join(s.Opt.Hosts, ", "))
	}
	tot := s.Totals()
	fmt.Fprintf(&b, "- 任务统计：成功 **%d**，警告 **%d**，失败 **%d**，跳过 %d\n\n",
		tot[StatusOK], tot[StatusWarn], tot[StatusFailed], tot[StatusSkipped])

	// 概览表
	b.WriteString("## 主机概览\n\n")
	b.WriteString("| 主机 | 角色 | 地址 | 负责人 | 成功 | 警告 | 失败 | 跳过 | 状态 |\n")
	b.WriteString("|------|------|------|--------|------|------|------|------|------|\n")
	hosts := append([]*HostResult(nil), s.Hosts...)
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Server.ID < hosts[j].Server.ID })
	for _, h := range hosts {
		ok, warn, failed, skipped := h.Counts()
		state := "✅ 正常"
		if h.Err != nil || failed > 0 {
			state = "❌ 异常"
		} else if warn > 0 {
			state = "⚠️ 注意"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %d | %d | %d | %s |\n",
			mdCell(h.Server.ID), mdCell(h.Identity.Role), mdCell(shortHost(h.Server.Host, h.Server.Port)),
			mdCell(h.Identity.Owner), ok, warn, failed, skipped, state)
	}
	b.WriteString("\n")

	// 明细
	b.WriteString("## 明细\n\n")
	for _, h := range hosts {
		fmt.Fprintf(&b, "### %s\n\n", h.Server.ID)
		id := h.Identity
		if id.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", id.Description)
		}
		var meta []string
		if id.Role != "" {
			meta = append(meta, "角色："+id.Role)
		}
		if h.Server.Group != "" {
			meta = append(meta, "分组："+h.Server.Group)
		}
		if id.Owner != "" {
			meta = append(meta, "负责人："+id.Owner)
		}
		if len(id.Tags) > 0 {
			meta = append(meta, "标签："+strings.Join(id.Tags, ", "))
		}
		if h.Conn != nil {
			meta = append(meta, fmt.Sprintf("登录：%s@%s", h.Conn.Username, shortHost(h.Server.Host, h.Server.Port)))
		}
		if len(meta) > 0 {
			fmt.Fprintf(&b, "%s\n\n", strings.Join(meta, " ｜ "))
		}
		for _, n := range id.Notes {
			fmt.Fprintf(&b, "- 备注：%s\n", n)
		}
		if len(id.Notes) > 0 {
			b.WriteString("\n")
		}
		if h.Err != nil {
			fmt.Fprintf(&b, "**连接失败**：%s\n\n", h.Err)
		}
		if len(h.Results) > 0 {
			b.WriteString("| 工作内容 | 说明 | 状态 | 耗时 | 结果 |\n")
			b.WriteString("|----------|------|------|------|------|\n")
			for _, r := range h.Results {
				var parts []string
				if r.Detail != "" {
					parts = append(parts, r.Detail)
				}
				for _, a := range r.Alerts {
					parts = append(parts, "⚠ "+a)
				}
				if r.Err != nil {
					parts = append(parts, r.Err.Error())
				}
				note := strings.Join(parts, "；")
				if note == "" {
					note = r.Detail
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
					mdCell(r.Task.Name), mdCell(r.Task.Description), statusIcon(r.Status),
					r.Duration.Round(time.Millisecond), mdCell(note))
			}
			b.WriteString("\n")
		}
	}

	// 服务器身份清单
	if includeHosts || cfg.Report.Host {
		b.WriteString("## 服务器清单\n\n")
		b.WriteString("| ID | 介绍 | 角色 | 分组 | 负责人 | 地址 | 标签 |\n")
		b.WriteString("|----|------|------|------|--------|------|------|\n")
		for i := range cfg.Servers {
			srv := &cfg.Servers[i]
			idt := cfg.EffectiveIdentity(srv)
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
				mdCell(srv.ID), mdCell(idt.Description), mdCell(idt.Role), mdCell(srv.Group),
				mdCell(idt.Owner), mdCell(shortHost(srv.Host, srv.Port)), mdCell(strings.Join(idt.Tags, ", ")))
		}
		b.WriteString("\n")
	}

	// 告警
	var alerts []string
	for _, h := range hosts {
		for _, r := range h.Results {
			for _, a := range r.Alerts {
				alerts = append(alerts, fmt.Sprintf("- **%s / %s**：%s", h.Server.ID, r.Task.Name, a))
			}
			if len(r.Alerts) == 0 && r.Status == StatusWarn && r.Detail != "" {
				alerts = append(alerts, fmt.Sprintf("- **%s / %s**：%s", h.Server.ID, r.Task.Name, r.Detail))
			}
		}
	}
	if len(alerts) > 0 {
		b.WriteString("## 需要关注的告警\n\n")
		b.WriteString(strings.Join(alerts, "\n"))
		b.WriteString("\n\n")
	}

	b.WriteString("---\n\n*由 sshex 生成*\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func statusIcon(s TaskStatus) string {
	switch s {
	case StatusOK:
		return "✅"
	case StatusWarn:
		return "⚠️"
	case StatusFailed:
		return "❌"
	case StatusSkipped:
		return "⏭️"
	}
	return string(s)
}

// mdCell 转义 Markdown 表格里的竖线与换行。
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", "<br>")
	s = strings.ReplaceAll(s, "\r", "")
	if s == "" {
		return " "
	}
	return s
}
