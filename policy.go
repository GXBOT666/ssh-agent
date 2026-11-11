package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// verbose / dryRun / jsonOut 由命令行参数设置。
var (
	verbose bool
	dryRun  bool
	jsonOut bool // 为 true 时抑制逐机进度输出，保证 stdout 只有 JSON
)

// ============================================================================
// 执行策略
//
// 按使用者要求：不做任何限制。
//   - 不拦截任何命令（包括 rm -rf /、reboot、mkfs 等）
//   - 不需要任何确认，AI 想执行什么就执行什么
//
// 保留本文件是为了审计日志与输出格式化。
// ============================================================================

// ============================================================================
// 审计日志：JSON Lines
// ============================================================================

var auditMu sync.Mutex

func audit(cfg *Config, s *Server, user, cmd string, exit int, d time.Duration, err error) {
	if cfg.Policy.AuditLog == "" {
		return
	}
	rec := map[string]any{
		"time":     time.Now().Format(time.RFC3339),
		"host":     s.ID,
		"addr":     shortHost(s.Host, s.Port),
		"user":     user,
		"command":  cmd,
		"exit":     exit,
		"duration": d.Round(time.Millisecond).String(),
	}
	if err != nil {
		rec["error"] = err.Error()
	}
	line, _ := json.Marshal(rec)
	auditMu.Lock()
	defer auditMu.Unlock()
	if err := os.MkdirAll(dirOf(cfg.Policy.AuditLog), 0o755); err == nil {
		if f, err := os.OpenFile(cfg.Policy.AuditLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
}

func dirOf(p string) string {
	i := strings.LastIndexAny(p, "/\\")
	if i <= 0 {
		return "."
	}
	return p[:i]
}

// humanBytes 友好的容量显示。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
