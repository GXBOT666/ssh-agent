package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

// ============================================================================
// 单个任务在一台服务器上的执行结果
// ============================================================================

type TaskStatus string

const (
	StatusOK      TaskStatus = "ok"
	StatusFailed  TaskStatus = "failed"
	StatusSkipped TaskStatus = "skipped"
	StatusWarn    TaskStatus = "warn"
)

type TaskResult struct {
	Task      *Task
	Server    *Server
	Status    TaskStatus
	ExitCode  int
	Stdout    string
	Stderr    string
	Err       error
	Duration  time.Duration
	Detail    string   // 一句话摘要（如"最高 / 91%"）
	Alerts    []string // 超过告警阈值的条目
	Metrics   map[string]float64
	Artifacts []string // 生成的本地文件
}

func (r *TaskResult) Summary() string {
	switch r.Status {
	case StatusOK:
		return "成功"
	case StatusWarn:
		return "警告"
	case StatusFailed:
		return "失败"
	case StatusSkipped:
		return "跳过"
	}
	return string(r.Status)
}

// renderTemplate 展开任务命令里的 {{.Var}}。
func renderTemplate(text string, vars map[string]string, s *Server, c *Conn) (string, error) {
	// 内置变量：Host=IP/域名, HostID=id, Name/HostName=显示名, User=登录用户, Port
	data := map[string]string{
		"Host": "", "HostID": "", "HostName": "", "Name": "", "User": "", "Port": "",
	}
	if s != nil {
		data["Host"] = s.Host
		data["HostID"] = s.ID
		data["HostName"] = s.Name
		data["Name"] = s.Name
		data["Port"] = strconv.Itoa(s.Port)
		if c != nil {
			data["User"] = c.Username
		}
	}
	// 任务自定义 vars 优先级最高，可覆盖内置变量
	for k, v := range vars {
		data[k] = v
	}
	t, err := template.New("cmd").Option("missingkey=zero").Parse(text)
	if err != nil {
		return "", fmt.Errorf("命令模板语法错误: %w", err)
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("命令模板渲染失败: %w", err)
	}
	// 未定义的变量渲染为空串（missingkey=zero），避免误伤命令里的正则/花括号
	return strings.ReplaceAll(b.String(), "<no value>", ""), nil
}

// ============================================================================
// 任务执行
// ============================================================================

// executor 负责把任务跑在执行器上。
type executor struct {
	cfg *Config
	// 下载文件名去重（并发时按 主机_任务 命名）
	downloadMu sync.Mutex
}

func newExecutor(cfg *Config) *executor { return &executor{cfg: cfg} }

// runTask 在 conn 上执行一个任务。
func (e *executor) runTask(ctx context.Context, conn *Conn, t *Task, overrideTimeout time.Duration) TaskResult {
	res := TaskResult{Task: t, Server: conn.Server, ExitCode: -1}
	timeout := durSec(t.TimeoutSec)
	if overrideTimeout > 0 {
		timeout = overrideTimeout
	}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	switch t.Type {
	case "command":
		e.runCommand(ctx, conn, t, timeout, &res)
	case "upload":
		e.runUpload(ctx, conn, t, timeout, &res)
	case "download":
		e.runDownload(ctx, conn, t, timeout, &res)
	default:
		res.Status = StatusSkipped
		res.Err = fmt.Errorf("未知任务类型 %q", t.Type)
	}
	return res
}

func (e *executor) runCommand(ctx context.Context, conn *Conn, t *Task, timeout time.Duration, res *TaskResult) {
	cmd, err := renderTemplate(t.Command, t.Vars, conn.Server, conn)
	if err != nil {
		res.Status = StatusFailed
		res.Err = err
		return
	}

	r := conn.Run(ctx, cmd, timeout)
	res.ExitCode = r.ExitCode
	res.Stdout = r.Stdout
	res.Stderr = r.Stderr

	if verbose {
		audit(e.cfg, conn.Server, conn.Username, cmd, r.ExitCode, r.Duration, r.Err)
	}

	if r.Err != nil {
		res.Status = StatusFailed
		res.Err = r.Err
		return
	}

	// 检查退出码
	if t.ExitCode != nil && r.ExitCode != *t.ExitCode {
		res.Status = StatusFailed
		res.Err = fmt.Errorf("退出码 %d，期望 %d", r.ExitCode, *t.ExitCode)
		if strings.TrimSpace(r.Stderr) != "" {
			res.Err = fmt.Errorf("%v\nstderr: %s", res.Err, strings.TrimSpace(r.Stderr))
		}
		return
	}
	// 检查期望输出
	if t.Expect != "" && !strings.Contains(r.Stdout, t.Expect) {
		res.Status = StatusFailed
		res.Err = fmt.Errorf("输出中未找到期望内容 %q", t.Expect)
		return
	}

	// 阈值判断
	res.Status = StatusOK
	if trimmed := strings.TrimSpace(r.Stderr); trimmed != "" {
		res.Status = StatusWarn
		res.Detail = "命令成功但有 stderr 输出: " + firstLine(trimmed)
	}

	// 巡检解析（若配置了 inspect）：可能把状态降级为 warn
	if t.Inspect != nil {
		applyInspect(e.cfg, t, res, conn.Server.ID)
	}

	// 保存输出
	if t.OutputFile != "" {
		path := expandHost(t.OutputFile, conn.Server, t.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			res.Status = StatusFailed
			res.Err = err
			return
		}
		body := strings.Join([]string{
			"# task: " + t.Name,
			"# host: " + conn.Server.ID + " (" + conn.Server.Host + ")",
			"# time: " + time.Now().Format(time.RFC3339),
			"# command:",
			cmd,
			"# stdout:",
			r.Stdout,
			"# stderr:",
			r.Stderr,
			"# exit: " + strconv.Itoa(r.ExitCode),
			"",
		}, "\n")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			res.Status = StatusFailed
			res.Err = fmt.Errorf("保存输出失败: %w", err)
			return
		}
		res.Artifacts = append(res.Artifacts, path)
	}

}

// firstLine 取第一行并限长，用于简洁提示。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// renderDry 生成 dry-run 展示用的命令文本。
func renderDry(t *Task, s *Server) string {
	switch t.Type {
	case "upload":
		return fmt.Sprintf("[dry-run] 上传 %s -> %s", t.Local, t.Remote)
	case "download":
		return fmt.Sprintf("[dry-run] 下载 %s -> %s", t.Remote, t.Local)
	default:
		cmd, err := renderTemplate(t.Command, t.Vars, s, nil)
		if err != nil {
			return "[模板错误] " + err.Error()
		}
		return "[dry-run] " + cmd
	}
}

// expandHost 把路径里的 {{.Host}} {{.Task}} 换成实际值。
func expandHost(p string, s *Server, task string) string {
	r := strings.NewReplacer("{{.Host}}", sanitizeName(s.ID), "{{.HostID}}", sanitizeName(s.ID), "{{.Task}}", sanitizeName(task))
	return r.Replace(p)
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// ---------------- 文件传输 ----------------

func (e *executor) runUpload(ctx context.Context, conn *Conn, t *Task, timeout time.Duration, res *TaskResult) {
	local := e.cfg.resolvePath(t.Local)
	data, err := os.ReadFile(local)
	if err != nil {
		res.Status = StatusFailed
		res.Err = fmt.Errorf("读取本地文件失败: %w", err)
		return
	}
	mode, err := parseMode(t.Mode)
	if err != nil {
		res.Status = StatusFailed
		res.Err = err
		return
	}
	// 上传只做 dry-run 与显式 deny 检查（不涉及确认流程）
	if dryRun {
		res.Status = StatusSkipped
		res.Detail = fmt.Sprintf("[dry-run] 将上传 %s -> %s", local, t.Remote)
		return
	}

	start := time.Now()
	err = uploadBytes(conn, data, t.Remote, mode)
	dur := time.Since(start)
	audit(e.cfg, conn.Server, conn.Username, "upload "+local+" -> "+t.Remote+" ("+strconv.Itoa(len(data))+" bytes)", 0, dur, err)
	if err != nil {
		res.Status = StatusFailed
		res.Err = err
		return
	}
	res.Status = StatusOK
	res.Detail = fmt.Sprintf("已上传 %s -> %s (%s)", filepath.Base(local), t.Remote, humanBytes(int64(len(data))))

	if t.ExecAfter != "" {
		cmd, err := renderTemplate(t.ExecAfter, t.Vars, conn.Server, conn)
		if err != nil {
			res.Status = StatusFailed
			res.Err = err
			return
		}
		r := conn.Run(ctx, cmd, timeout)
		res.Stdout = r.Stdout
		res.Stderr = r.Stderr
		if r.Err != nil || r.ExitCode != 0 {
			res.Status = StatusFailed
			res.Err = fmt.Errorf("上传后命令执行失败: %v exit=%d", r.Err, r.ExitCode)
		}
	}
}

func (e *executor) runDownload(ctx context.Context, conn *Conn, t *Task, timeout time.Duration, res *TaskResult) {
	if t.Remote == "" {
		res.Status = StatusFailed
		res.Err = fmt.Errorf("download 缺少 remote")
		return
	}
	// 目标文件名：若 local 是目录则以远端文件名落盘
	local := e.cfg.resolvePath(expandHost(t.Local, conn.Server, t.Name))
	if st, err := os.Stat(local); err == nil && st.IsDir() {
		local = filepath.Join(local, filepath.Base(t.Remote))
	}
	if _, err := os.Stat(local); err == nil && !*t.Overwrite {
		res.Status = StatusSkipped
		res.Detail = "本地文件已存在，overwrite=false"
		return
	}
	start := time.Now()
	data, mode, err := downloadFile(conn, t.Remote)
	dur := time.Since(start)
	audit(e.cfg, conn.Server, conn.Username, "download "+t.Remote+" -> "+local, 0, dur, err)
	if err != nil {
		res.Status = StatusFailed
		res.Err = err
		return
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		res.Status = StatusFailed
		res.Err = err
		return
	}
	// 保留远端原始权限
	perm := os.FileMode(0o644)
	if mode&os.ModePerm != 0 {
		perm = mode & os.ModePerm
	}
	if err := os.WriteFile(local, data, perm); err != nil {
		res.Status = StatusFailed
		res.Err = err
		return
	}
	res.Status = StatusOK
	res.Artifacts = append(res.Artifacts, local)
	res.Detail = fmt.Sprintf("已下载 %s -> %s (%s)", t.Remote, local, humanBytes(int64(len(data))))
}

// uploadBytes 通过 SFTP 写入远端文件。若远端没有 sftp-server，自动退化为 base64 管道。
func uploadBytes(conn *Conn, data []byte, remote string, mode os.FileMode) error {
	if err := sftpUpload(conn, data, remote, mode); err == nil {
		return nil
	} else if !strings.Contains(err.Error(), "sftp") && !isSFTPUnavailable(err) {
		return err
	}
	// 退化方案：base64 通过 shell 写入
	tmp := remote + ".sshex.tmp"
	payload := base64.StdEncoding.EncodeToString(data)
	const chunk = 60000
	var script strings.Builder
	fmt.Fprintf(&script, "umask 077; : > %s || exit 1\n", shQuoteRemote(tmp))
	for i := 0; i < len(payload); i += chunk {
		end := i + chunk
		if end > len(payload) {
			end = len(payload)
		}
		fmt.Fprintf(&script, "printf '%%s' %s | base64 -d >> %s || exit 1\n", shQuoteRemote(payload[i:end]), shQuoteRemote(tmp))
	}
	fmt.Fprintf(&script, "chmod %o %s && mv -f %s %s || { rm -f %s; exit 1; }\n", mode.Perm(), shQuoteRemote(tmp), shQuoteRemote(tmp), shQuoteRemote(remote), shQuoteRemote(tmp))
	r := conn.Run(context.Background(), script.String(), 5*time.Minute)
	if r.Err != nil {
		return r.Err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("写出远端文件失败 (exit=%d): %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}

// shQuoteRemote 给远端路径加单引号。
func shQuoteRemote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func downloadFile(conn *Conn, remote string) ([]byte, os.FileMode, error) {
	data, mode, err := sftpDownload(conn, remote)
	if err == nil {
		return data, mode, nil
	}
	if !isSFTPUnavailable(err) {
		return nil, 0, err
	}
	r := conn.Run(context.Background(), "base64 < "+shQuoteRemote(remote), 5*time.Minute)
	if r.Err != nil {
		return nil, 0, r.Err
	}
	if r.ExitCode != 0 {
		return nil, 0, fmt.Errorf("读取远端文件失败 (exit=%d): %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(r.Stdout), ""))
	if err != nil {
		return nil, 0, fmt.Errorf("解码远端文件失败: %w", err)
	}
	return b, 0o644, nil
}
