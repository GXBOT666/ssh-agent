package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// ============================================================================
// 认证方法构建
// ============================================================================

// loadPrivateKey 读取并解析私钥（支持带口令）。
func loadPrivateKey(k *Key) (ssh.Signer, error) {
	var pemData []byte
	var err error
	if k.Path != "" {
		pemData, err = os.ReadFile(k.Path)
		if err != nil {
			return nil, fmt.Errorf("读取私钥 %s 失败: %w", k.Path, err)
		}
	} else {
		pemData = []byte(k.Value)
		// JSON 里的 \n 转义已由 JSON 解析处理；若有人写成一行也兼容
		if !bytes.Contains(pemData, []byte("\n")) {
			pemData = []byte(strings.ReplaceAll(string(pemData), "\\n", "\n"))
		}
	}

	signer, err := ssh.ParsePrivateKey(pemData)
	if err == nil {
		return signer, nil
	}
	var needPass *ssh.PassphraseMissingError
	if errors.As(err, &needPass) || strings.Contains(err.Error(), "encrypted") {
		pass := k.Passphrase
		if pass == "" && isTerminal() {
			pass, err = readSecretLine(os.Stdin, fmt.Sprintf("请输入私钥 %s 的口令: ", keyLabel(k)))
			if err != nil {
				return nil, err
			}
		}
		if pass == "" {
			return nil, fmt.Errorf("私钥 %s 已加密，但未提供 passphrase（可写 passphrase 或环境变量）", keyLabel(k))
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(pemData, []byte(pass))
		if err != nil {
			return nil, fmt.Errorf("私钥口令错误或私钥损坏 %s: %w", keyLabel(k), err)
		}
		return signer, nil
	}
	return nil, fmt.Errorf("解析私钥失败 %s: %w（支持 OpenSSH / RSA / ECDSA / Ed25519 格式，可先用 ssh-keygen -p -m PEM -f 转换）", keyLabel(k), err)
}

func keyLabel(k *Key) string {
	if k.Path != "" {
		return fmt.Sprintf("%s(%s)", k.Name, filepath.Base(k.Path))
	}
	return k.Name
}

// loadingCache 让私钥在一批任务中只解析一次。
type loadingCache struct {
	mu sync.Mutex
	m  map[string]ssh.Signer
}

func newLoadingCache() *loadingCache { return &loadingCache{m: map[string]ssh.Signer{}} }

func (c *loadingCache) signer(k *Key) (ssh.Signer, error) {
	id := k.Name + "|" + k.Path + "|" + sha256Short(k.Value) + "|" + sha256Short(k.Passphrase)
	c.mu.Lock()
	if s, ok := c.m[id]; ok {
		c.mu.Unlock()
		return s, nil
	}
	c.mu.Unlock()
	s, err := loadPrivateKey(k)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.m[id] = s
	c.mu.Unlock()
	return s, nil
}

// authMethodsFor 根据密钥构建认证方法列表。
func (c *Config) authMethodsFor(keyName string, cache *loadingCache) ([]ssh.AuthMethod, error) {
	k, ok := findKey(c.Keys, keyName)
	if !ok {
		return nil, fmt.Errorf("密钥 %q 不存在", keyName)
	}
	t, err := k.resolveType()
	if err != nil {
		return nil, err
	}
	switch t {
	case "none":
		return nil, nil
	case "password":
		pw := k.Value
		if pw == "" && isTerminal() {
			pw, err = readSecretLine(os.Stdin, fmt.Sprintf("请输入 %s 的密码: ", k.Name))
			if err != nil {
				return nil, err
			}
		}
		if pw == "" {
			return nil, fmt.Errorf("密钥 %q 是 password 类型但没有 value", k.Name)
		}
		return []ssh.AuthMethod{ssh.Password(pw)}, nil
	case "agent":
		sock := k.Value
		if sock == "" {
			sock = os.Getenv("SSH_AUTH_SOCK")
		}
		if sock == "" {
			return nil, fmt.Errorf("密钥 %q 是 agent 类型但未指定 socket，且 SSH_AUTH_SOCK 为空", k.Name)
		}
		nc, err := net.Dial("unix", sock)
		if err != nil {
			return nil, fmt.Errorf("连接 ssh-agent %s 失败: %w", sock, err)
		}
		// 注意：这里不关闭 nc，保持 agent 连接可用
		agentClient := agent.NewClient(nc)
		return []ssh.AuthMethod{ssh.PublicKeysCallback(agentClient.Signers)}, nil
	case "private_key":
		s, err := cache.signer(k)
		if err != nil {
			return nil, err
		}
		return []ssh.AuthMethod{ssh.PublicKeys(s)}, nil
	}
	return nil, fmt.Errorf("未知密钥类型 %q", t)
}

// usernameFor 决定登录用户：server.username > key.username > jump.username > 当前系统用户。
func (c *Config) usernameFor(s *Server, keyName string, fallback string) (string, error) {
	if s != nil && s.Username != "" {
		return s.Username, nil
	}
	if k, ok := findKey(c.Keys, keyName); ok && k.Username != "" {
		return k.Username, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username, nil
	}
	if v := os.Getenv("USER"); v != "" {
		return v, nil
	}
	return "", errors.New("无法确定登录用户名，请在 servers[].username 中指定")
}

// ============================================================================
// 拨号：直连 / 跳板机 / ProxyCommand
// ============================================================================

// dialer 负责建立到某台服务器的 TCP（或代理）连接。
type dialer struct {
	cfg   *Config
	cache *loadingCache
	// 跳板机连接缓存：同一批任务复用，避免反复登录
	jumpMu    sync.Mutex
	jumpConns map[string]*ssh.Client
}

func newDialer(cfg *Config) *dialer {
	return &dialer{cfg: cfg, cache: newLoadingCache(), jumpConns: map[string]*ssh.Client{}}
}

func (d *dialer) Close() {
	d.jumpMu.Lock()
	defer d.jumpMu.Unlock()
	for k, c := range d.jumpConns {
		c.Close()
		delete(d.jumpConns, k)
	}
}

// dial 建立到 server:port 的 net.Conn，可能是经过跳板的。
func (d *dialer) dial(ctx context.Context, targetHost string, targetPort int, via *Server, viaUser, viaKey string, directJump *Jump) (net.Conn, string, error) {
	addr := net.JoinHostPort(targetHost, strconv.Itoa(targetPort))

	// 自定义 ProxyCommand（字面量）
	if via != nil && via.Proxy != "" {
		if via.ProxyTokens != nil {
			return nil, "", errors.New("内部错误：proxy 配置冲突")
		}
		cmd := replaceProxyPlaceholders(via.Proxy, targetHost, targetPort, viaUser, d.keyPathOf(viaKey))
		nc, err := dialProxyCommand(ctx, strings.Fields(cmd))
		return nc, "proxy_command", err
	}
	if via != nil && len(via.ProxyTokens) > 0 {
		var argv []string
		for _, tok := range via.ProxyTokens {
			argv = append(argv, replaceProxyPlaceholders(tok, targetHost, targetPort, viaUser, d.keyPathOf(viaKey)))
		}
		nc, err := dialProxyCommand(ctx, argv)
		return nc, "proxy_command", err
	}

	// 跳板机
	if directJump != nil {
		jc, err := d.jumpClient(ctx, directJump, "")
		if err != nil {
			return nil, "", err
		}
		nc, err := jc.Dial("tcp", addr)
		return nc, "via " + jumpLabel(directJump), err
	}
	if via != nil && via.Jump != nil {
		jc, err := d.jumpClient(ctx, via.Jump, via.ID)
		if err != nil {
			return nil, "", err
		}
		nc, err := jc.Dial("tcp", addr)
		return nc, "via " + jumpLabel(via.Jump), err
	}

	var nd net.Dialer
	nc, err := nd.DialContext(ctx, "tcp", addr)
	return nc, "direct", err
}

func jumpLabel(j *Jump) string {
	if j.Server != "" {
		return "jump:" + j.Server
	}
	return "jump:" + net.JoinHostPort(j.Host, strconv.Itoa(j.Port))
}

// jumpClient 取得（或建立）跳板机的 ssh.Client。
func (d *dialer) jumpClient(ctx context.Context, j *Jump, selfID string) (*ssh.Client, error) {
	cacheKey := jumpLabel(j) + "|" + j.Username + "|" + j.Key

	d.jumpMu.Lock()
	if c, ok := d.jumpConns[cacheKey]; ok {
		d.jumpMu.Unlock()
		return c, nil
	}
	d.jumpMu.Unlock()

	var host, keyName, username string
	var port int
	var hostKeyCfg *Server

	if j.Server != "" {
		js := d.cfg.serverByID(j.Server)
		if js == nil {
			return nil, fmt.Errorf("跳板机 %q 不存在", j.Server)
		}
		if js.ID == selfID {
			return nil, fmt.Errorf("跳板机不能是它自己: %s", js.ID)
		}
		if !truthy(js.Enabled) {
			return nil, fmt.Errorf("跳板机 %q 已被禁用", j.Server)
		}
		host, port, keyName = js.Host, js.Port, js.Key
		username, _ = d.cfg.usernameFor(js, keyName, "")
		hostKeyCfg = js
	} else {
		host, port, keyName = j.Host, j.Port, j.Key
		if port == 0 {
			port = 22
		}
		username, _ = d.cfg.usernameFor(nil, keyName, j.Username)
		if j.Username != "" {
			username = j.Username
		}
		hostKeyCfg = &Server{
			ID:      "jump",
			Host:    host,
			Port:    port,
			HostKey: &HostKey{Policy: "accept-new", KnownHosts: d.cfg.defaultKnownHosts()},
		}
	}

	nc, _, err := d.dial(ctx, host, port, hostKeyCfg, username, keyName, nil)
	if err != nil {
		return nil, fmt.Errorf("连接跳板机 %s 失败: %w", net.JoinHostPort(host, strconv.Itoa(port)), err)
	}

	client, _, err := d.handshake(ctx, nc, hostKeyCfg, username, keyName, 20*time.Second)
	if err != nil {
		return nil, fmt.Errorf("跳板机 %s 登录失败: %w", host, err)
	}

	d.jumpMu.Lock()
	d.jumpConns[cacheKey] = client
	d.jumpMu.Unlock()
	return client, nil
}

func (d *dialer) keyPathOf(keyName string) string {
	if k, ok := findKey(d.cfg.Keys, keyName); ok {
		return k.Path
	}
	return ""
}

// defaultKnownHosts 统一放在数据目录，不写 ~/.ssh。
func (c *Config) defaultKnownHosts() string {
	if c != nil {
		return c.sshKnownHostsPath()
	}
	return filepath.Join(dataHome(), "known_hosts")
}

func replaceProxyPlaceholders(s string, host string, port int, user, keyPath string) string {
	r := strings.NewReplacer(
		"%h", host,
		"%p", strconv.Itoa(port),
		"%r", user,
		"%k", keyPath,
		"%%", "%",
	)
	return r.Replace(s)
}

// dialProxyCommand 启动外部命令并把它当管道用（类似 ssh -o ProxyCommand）。
func dialProxyCommand(ctx context.Context, argv []string) (net.Conn, error) {
	if len(argv) == 0 {
		return nil, errors.New("proxy_command 为空")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 ProxyCommand 失败: %w", err)
	}
	return &pipeConn{r: stdout, w: stdin, cmd: cmd}, nil
}

// pipeConn 把子进程的标准输入输出包装成 net.Conn。
type pipeConn struct {
	r   io.ReadCloser
	w   io.WriteCloser
	cmd *exec.Cmd
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeConn) Close() error {
	p.w.Close()
	p.r.Close()
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	return p.cmd.Wait()
}
func (p *pipeConn) LocalAddr() net.Addr              { return dummyAddr("local") }
func (p *pipeConn) RemoteAddr() net.Addr             { return dummyAddr("remote") }
func (p *pipeConn) SetDeadline(time.Time) error      { return nil }
func (p *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (p *pipeConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr string

func (d dummyAddr) Network() string { return "pipe" }
func (d dummyAddr) String() string  { return string(d) }

// ============================================================================
// 握手与连接对象
// ============================================================================

// handshake 在已建立的 net.Conn 上完成 SSH 握手。
func (d *dialer) handshake(ctx context.Context, nc net.Conn, s *Server, username, keyName string, timeout time.Duration) (*ssh.Client, *hostKeyVerifier, error) {
	auths, err := d.cfg.authMethodsFor(keyName, d.cache)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	verifier := newHostKeyVerifier(s)

	cc := &ssh.ClientConfig{
		User:            username,
		Auth:            auths,
		HostKeyCallback: verifier.Callback(),
		Timeout:         timeout,
	}

	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		conn, chans, reqs, err := ssh.NewClientConn(nc, net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), cc)
		if err != nil {
			ch <- result{nil, decorateAuthErr(err, username, keyName, verifier)}
			return
		}
		ch <- result{ssh.NewClient(conn, chans, reqs), nil}
	}()

	var res result
	select {
	case res = <-ch:
	case <-ctx.Done():
		nc.Close()
		return nil, nil, fmt.Errorf("连接超时: %w", ctx.Err())
	}
	if res.err != nil {
		nc.Close()
		return nil, nil, res.err
	}
	return res.client, verifier, nil
}

func decorateAuthErr(err error, username, keyName string, v *hostKeyVerifier) error {
	if v.missed && v.seen.SHA256 != "" {
		// 主机密钥问题已在回调里返回详细错误
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, "unable to authenticate") || strings.Contains(msg, "no supported methods") {
		return fmt.Errorf("认证失败（用户 %s，密钥 %s）：请确认公钥已加入目标机 ~/.ssh/authorized_keys，或密码/私钥正确。原始错误: %v", username, keyName, err)
	}
	return err
}

// Conn 是面向一台服务器的高层连接封装。
type Conn struct {
	Cfg       *Config
	Server    *Server
	Username  string
	KeyName   string
	HostKey   HostKeySeen
	Transport string // direct / via jump:xxx / proxy_command

	client *ssh.Client
	dialT  *dialer // 仅跳板机路径下持有
	via    *Server
	isJump bool

	mu       sync.Mutex
	lastUsed time.Time
	dialed   time.Time

	sftpMu sync.Mutex
	sftp   *sftp.Client
}

func (c *Conn) Client() *ssh.Client { return c.client }

// DialTimeout 用的 context 超时。
func (d *dialer) dialTimeout(s *Server) time.Duration {
	if s.TimeoutSec > 0 {
		return durSec(s.TimeoutSec)
	}
	return 15 * time.Second
}

// Connect 建立到服务器的 SSH 连接。
func (d *dialer) Connect(ctx context.Context, s *Server) (*Conn, error) {
	if !truthy(s.Enabled) {
		return nil, fmt.Errorf("服务器 %s 已被禁用（enabled=false）", s.ID)
	}
	username, err := d.cfg.usernameFor(s, s.Key, "")
	if err != nil {
		return nil, err
	}

	timeout := d.dialTimeout(s)
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	nc, transport, err := d.dial(dctx, s.Host, s.Port, s, username, s.Key, nil)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), err)
	}

	client, verifier, err := d.handshake(dctx, nc, s, username, s.Key, timeout)
	if err != nil {
		return nil, err
	}

	conn := &Conn{
		Cfg: d.cfg, Server: s, Username: username, KeyName: s.Key,
		Transport: transport, client: client, dialT: d,
		lastUsed: time.Now(), dialed: start,
	}
	if verifier != nil && verifier.seen.SHA256 != "" {
		conn.HostKey = verifier.seen
	}
	conn.startKeepAlive()
	return conn, nil
}

// startKeepAlive 定期发送 keepalive 请求，及时发现断线。
func (c *Conn) startKeepAlive() {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			c.mu.Lock()
			cl := c.client
			c.mu.Unlock()
			if cl == nil {
				return
			}
			_, _, err := cl.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				// 连接已断开；真正需要时会重连
				return
			}
		}
	}()
}

// Touch 记录使用时间。
func (c *Conn) Touch() {
	c.mu.Lock()
	c.lastUsed = time.Now()
	c.mu.Unlock()
}

// Close 关闭连接。
func (c *Conn) Close() error {
	c.mu.Lock()
	cl := c.client
	c.client = nil
	c.mu.Unlock()
	c.closeSFTP()
	if cl != nil {
		return cl.Close()
	}
	return nil
}

// runResult 是一次远端命令的结果。
type runResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error // 传输层错误（非零退出不算）
	Duration time.Duration
	TimedOut bool
}

// Run 在服务器的默认 shell 中执行命令（bash -lc 优先，退化为 sh -c / cmd）。
func (c *Conn) Run(ctx context.Context, command string, timeout time.Duration) runResult {
	r := runResult{ExitCode: -1}
	start := time.Now()
	defer func() { r.Duration = time.Since(start) }()

	if strings.TrimSpace(command) == "" {
		r.Err = errors.New("命令为空")
		return r
	}

	sess, err := c.client.NewSession()
	if err != nil {
		r.Err = fmt.Errorf("创建会话失败（连接可能已断开）: %w", err)
		return r
	}
	defer sess.Close()

	var outBuf, errBuf bytes.Buffer
	sess.Stdout = &limitedWriter{w: &outBuf, limit: 8 << 20}
	sess.Stderr = &limitedWriter{w: &errBuf, limit: 1 << 20}
	sess.Stdin = strings.NewReader("")

	wrapped := wrapCommand(command)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		err := sess.Start(wrapped)
		if err != nil {
			done <- err
			return
		}
		done <- sess.Wait()
	}()

	select {
	case err := <-done:
		r.Stdout = outBuf.String()
		r.Stderr = errBuf.String()
		if err == nil {
			r.ExitCode = 0
			return r
		}
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			r.ExitCode = ee.ExitStatus()
			if ee.Signal() != "" {
				r.Stderr += fmt.Sprintf("\n[远端进程被信号 %s 终止]\n", ee.Signal())
			}
			return r
		}
		var me *ssh.ExitMissingError
		if errors.As(err, &me) {
			r.ExitCode = -2
			r.Err = errors.New("远端进程异常退出（未返回退出码，可能被 kill 或连接中断）")
			return r
		}
		r.Err = err
		return r
	case <-runCtx.Done():
		r.TimedOut = true
		r.Err = fmt.Errorf("执行超时（%s），已尝试终止远端进程", timeout)
		// 尝试结束远端会话
		_ = sess.Signal(ssh.SIGTERM)
		go func() {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = sess.Close()
			}
		}()
		r.Stdout = outBuf.String()
		r.Stderr = errBuf.String()
		r.ExitCode = -3
		return r
	}
}

// wrapCommand 把用户命令包装成"登录 shell 执行"的形式：
// bash -lc 能拿到交互式登录时的 PATH 与 /etc/profile 环境变量，
// 命令用 base64 传递，避免引号、换行、中文等被二次解析。
func wrapCommand(command string) string {
	payload := base64.StdEncoding.EncodeToString([]byte(command))
	return fmt.Sprintf("bash -l -c \"$(echo %s | base64 -d)\"", payload)
}

// limitedWriter 限制缓冲大小，防止远端狂刷日志把内存吃光。
type limitedWriter struct {
	w       io.Writer
	limit   int
	written int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	remain := l.limit - l.written
	if remain <= 0 {
		return len(p), nil // 超出上限的输出直接丢弃
	}
	if len(p) > remain {
		if _, err := l.w.Write(p[:remain]); err != nil {
			return 0, err
		}
		l.written = l.limit
		if _, err := l.w.Write([]byte("\n[输出过长，已截断]\n")); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	n, err := l.w.Write(p)
	l.written += n
	return n, err
}
