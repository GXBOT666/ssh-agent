package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// ============================================================================
// 主机公钥指纹：解析、比对、known_hosts 读写
//
// 说明：target 包里有 knownhosts，但为了支持"写 SHA256 指纹"以及把新
// 指纹落盘到 JSON 配置，这里自己实现一份小而清晰的解析逻辑。
// ============================================================================

// HostKeySeen 是连接时观察到的主机公钥结果。
type HostKeySeen struct {
	Key    ssh.PublicKey
	SHA256 string // SHA256:xxxx (OpenSSH 格式)
	MD5    string // MD5:xx:...（旧格式，仅用于兼容）
	Line   string // authorized_keys 格式: "ssh-ed25519 AAAA..."
}

func newHostKeySeen(k ssh.PublicKey) HostKeySeen {
	return HostKeySeen{
		Key:    k,
		SHA256: NormalizeFingerprint(k),
		MD5:    ssh.FingerprintLegacyMD5(k),
		Line:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))),
	}
}

// NormalizeFingerprint 计算公钥的 SHA256 指纹。
func NormalizeFingerprint(k ssh.PublicKey) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sha256Sum(k.Marshal()))
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// parseFingerprint 把配置里的指纹字符串标准化为 "SHA256:xxx"
// 或 authorized_keys 公钥行，无法识别时返回错误。
// 返回值 kind 为 "sha256" | "key"。
func parseFingerprint(s string) (normalized string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("空指纹")
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "sha256:") {
		raw := strings.TrimSpace(s[len("sha256:"):])
		// 容忍用户粘贴带 = 填充的写法
		raw = strings.TrimRight(raw, "=")
		b, err := base64.RawStdEncoding.DecodeString(raw)
		if err != nil {
			return "", fmt.Errorf("SHA256 指纹 base64 解码失败: %v", err)
		}
		if len(b) != sha256.Size {
			return "", fmt.Errorf("SHA256 指纹长度应为 32 字节，得到 %d", len(b))
		}
		return "SHA256:" + base64.RawStdEncoding.EncodeToString(b), nil
	}
	if strings.HasPrefix(lower, "md5:") {
		return "", errors.New("不支持 MD5 指纹，请改用 SHA256 指纹（ssh-keygen -lf 可查看）")
	}
	// 尝试当作公钥解析
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s)); err == nil {
		return s, nil
	}
	if _, err := ssh.ParsePublicKey([]byte(s)); err == nil {
		return s, nil
	}
	return "", errors.New("无法识别：应为 \"SHA256:...\" 或 \"ssh-ed25519 AAAA...\" 形式的公钥")
}

// hostKeyVerifier 实现 ssh.HostKeyCallback。
type hostKeyVerifier struct {
	policy     string
	trusted    []trustedHostKey
	knownHosts string
	addrKey    string // known_hosts 中使用的主机名（"host" 或 "[host]:port"）
	host       string
	port       int
	seen       HostKeySeen
	missed     bool
	verify     *bool
	seenMu     *sync.Mutex
}

type trustedHostKey struct {
	sha256 string
	key    ssh.PublicKey
}

func newHostKeyVerifier(s *Server) *hostKeyVerifier {
	hk := s.HostKey
	v := &hostKeyVerifier{
		policy:     hk.Policy,
		knownHosts: hk.KnownHosts,
		host:       s.Host,
		port:       s.Port,
		addrKey:    knownHostsKey(s.Host, s.Port),
		verify:     hk.Verify,
		seenMu:     &sync.Mutex{},
	}
	for _, fp := range hk.Fingerprints {
		n, err := parseFingerprint(fp)
		if err != nil {
			// validate() 已经拦过，这里兜底
			continue
		}
		if strings.HasPrefix(n, "SHA256:") {
			v.trusted = append(v.trusted, trustedHostKey{sha256: n})
		} else if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(n)); err == nil {
			v.trusted = append(v.trusted, trustedHostKey{key: pk})
		}
	}
	return v
}

// knownHostsKey 按 OpenSSH 规则生成主机名：非 22 端口写成 [host]:port。
func knownHostsKey(host string, port int) string {
	if port == 22 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (v *hostKeyVerifier) matchesTrusted(k ssh.PublicKey) bool {
	fp := NormalizeFingerprint(k)
	for _, t := range v.trusted {
		if t.sha256 != "" && t.sha256 == fp {
			return true
		}
		if t.key != nil && strings.EqualFold(string(t.key.Marshal()), string(k.Marshal())) {
			return true
		}
	}
	return false
}

// Callback 返回 ssh.HostKeyCallback。
func (v *hostKeyVerifier) Callback() ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		seen := newHostKeySeen(key)
		v.seenMu.Lock()
		v.seen = seen
		v.missed = false
		v.seenMu.Unlock()

		switch v.policy {
		case "insecure":
			return nil
		}
		if v.verify != nil && !*v.verify {
			return nil
		}

		// 1) 配置里的指纹
		if v.matchesTrusted(key) {
			return nil
		}

		// 2) known_hosts
		known, err := lookupKnownHosts(v.knownHosts, v.addrKey, v.host)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("读取 known_hosts 失败: %w", err)
		}
		if known.anyEntry {
			if known.trusted(key) {
				return nil
			}
			v.missed = true
			return fmt.Errorf(
				"主机密钥与 known_hosts 记录不一致！\n"+
					"  主机: %s\n"+
					"  本次: %s\n"+
					"  known_hosts: %s\n"+
					"如果确认服务器重装或更换了密钥，请删除对应行: sh -c 'ssh-keygen -R %s'，并更新配置里的 host_key.fingerprints",
				v.addrKey, seen.SHA256, strings.Join(known.matching(), "\n  "), v.addrKey)
		}
		if len(v.trusted) > 0 && !v.matchesTrusted(key) {
			v.missed = true
			return fmt.Errorf(
				"主机密钥与配置不匹配！\n  主机: %s\n  本次: %s\n  配置: %s",
				v.addrKey, seen.SHA256, joinTrusted(v.trusted))
		}

		// 3) 首次见到的密钥
		if v.policy == "accept-new" {
			if err := appendKnownHosts(v.knownHosts, v.addrKey, seen.Line); err != nil {
				fmt.Fprintf(os.Stderr, "警告: 记录主机密钥到 %s 失败: %v\n", v.knownHosts, err)
			} else if verbose {
				fmt.Fprintf(os.Stderr, "已记录主机 %s 的密钥 %s → %s\n", v.addrKey, seen.SHA256, v.knownHosts)
			}
			return nil
		}

		v.missed = true
		return fmt.Errorf(
			"未知主机密钥（policy=strict）\n"+
				"  主机: %s\n"+
				"  指纹: %s\n"+
				"  公钥: %s\n"+
				"请先核对指纹后再加入配置: servers[].host_key.fingerprints",
			v.addrKey, seen.SHA256, seen.Line)
	}
}

func joinTrusted(ts []trustedHostKey) string {
	var out []string
	for _, t := range ts {
		if t.sha256 != "" {
			out = append(out, t.sha256)
		} else if t.key != nil {
			out = append(out, ssh.FingerprintSHA256(t.key))
		}
	}
	return strings.Join(out, ", ")
}

// ============================================================================
// known_hosts 解析
// ============================================================================

type knownHostsResult struct {
	anyEntry bool
	lines    []string
	keys     []ssh.PublicKey
}

func (r knownHostsResult) trusted(k ssh.PublicKey) bool {
	for _, pk := range r.keys {
		if string(pk.Marshal()) == string(k.Marshal()) {
			return true
		}
	}
	return false
}

func (r knownHostsResult) matching() []string {
	if len(r.lines) == 0 {
		return []string{"(已知其他密钥类型)"}
	}
	return r.lines
}

// lookupKnownHosts 在 known_hosts 中查找 host 对应的公钥。
// 支持明文主机名、通配符、不支持的 @cert-authority / @revoked 行会被忽略（@revoked 会按不信任处理）。
func lookupKnownHosts(path, addrKey, plainHost string) (knownHostsResult, error) {
	var res knownHostsResult
	f, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		marker := ""
		if strings.HasPrefix(fields[0], "@") {
			marker = fields[0]
			fields = fields[1:]
		}
		if len(fields) < 3 {
			continue
		}
		hostsField, keyType, blob := fields[0], fields[1], fields[2]
		if !knownHostsHostMatch(hostsField, addrKey, plainHost) {
			continue
		}
		// 从 blob 解出公钥
		raw, err := base64.StdEncoding.DecodeString(blob)
		if err != nil {
			continue
		}
		pk, err := ssh.ParsePublicKey(raw)
		if err != nil || pk.Type() != keyType {
			continue
		}
		if marker == "@cert-authority" {
			continue
		}
		if marker == "@revoked" {
			// 被吊销的密钥：视为"存在但不信任"
			res.anyEntry = true
			return res, fmt.Errorf("主机密钥 %s 已被 known_hosts 中的 @revoked 标记为吊销", NormalizeFingerprint(pk))
		}
		res.anyEntry = true
		res.keys = append(res.keys, pk)
		res.lines = append(res.lines, NormalizeFingerprint(pk))
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	return res, nil
}

func knownHostsHostMatch(field, addrKey, plainHost string) bool {
	if strings.HasPrefix(field, "|1|") {
		return hashedHostMatch(field, addrKey) || hashedHostMatch(field, plainHost)
	}
	for _, pat := range strings.Split(field, ",") {
		if matchHostPattern(pat, addrKey) || matchHostPattern(pat, plainHost) {
			return true
		}
	}
	return false
}

// matchHostPattern 实现 OpenSSH 的 * 和 ? 通配符匹配（不区分大小写）。
func matchHostPattern(pattern, host string) bool {
	pattern = strings.ToLower(pattern)
	host = strings.ToLower(host)
	return globMatch(pattern, host)
}

func globMatch(pattern, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			star = pi
			mark = si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// hashedHostMatch 处理 |1|salt|hash 格式：OpenSSH 用 HMAC-SHA1(salt, host)。
func hashedHostMatch(field, host string) bool {
	parts := strings.Split(field, "|")
	if len(parts) != 4 || parts[1] != "1" {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(parts[2])
	want, err2 := base64.StdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return hmac.Equal(mac.Sum(nil), want)
}

// appendKnownHosts 追加一条记录（文件不存在则创建，权限 0600）。
func appendKnownHosts(path, hostKey, pubLine string) error {
	if path == "" {
		return errors.New("known_hosts 路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	line := fmt.Sprintf("%s %s\n", hostKey, pubLine)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// 确保文件末尾有换行，避免拼接
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		data, _ := os.ReadFile(path)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			line = "\n" + line
		}
	}
	_, err = f.WriteString(line)
	return err
}
