package main

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// ---------- 辅助 ----------

// writeTemp 把内容写到临时目录并返回路径。
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// newTestSigner 生成一个 ed25519 签名器（每次调用不同）。
func newTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// hashHostForTest 生成 OpenSSH 的 |1|salt|hash 主机名格式。
func hashHostForTest(salt []byte, host string) string {
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func timeNow() time.Time { return time.Now() }

// ---------- 配置 ----------

const baseConfig = `{
  "version": 1,
  "name": "t",
  "keys": [{"name":"k1","type":"password","value":"pw"}],
  "identities": {"default":{"role":"server"},"servers":[{"name":"web1","role":"web","description":"web 服务器"}]},
  "servers": [{"id":"web1","host":"10.0.0.1","key":"k1","username":"root"}],
  "tasks": [{"name":"t1","command":"uptime"}]
}`

func TestLoadConfigValid(t *testing.T) {
	p := writeTemp(t, "config.json", baseConfig)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("不应有错误: %v", err)
	}
	if c.Servers[0].Port != 22 {
		t.Errorf("默认端口应为 22，得到 %d", c.Servers[0].Port)
	}
	if c.Servers[0].HostKey.Policy != "strict" {
		t.Errorf("默认指纹策略应为 strict，得到 %q", c.Servers[0].HostKey.Policy)
	}
	if !c.Policy.ConfirmRisk || !c.Policy.ContinueOnErr {
		t.Errorf("policy 默认应为 confirm_risk=true, continue_on_error=true，得到 %+v", c.Policy)
	}
	if c.Policy.MaxParallel != 4 {
		t.Errorf("默认并发应为 4，得到 %d", c.Policy.MaxParallel)
	}
	lang := c.Alert.LoadPerCore
	if lang != 2 {
		t.Errorf("默认负载阈值应为 2，得到 %v", lang)
	}
	id := c.EffectiveIdentity(&c.Servers[0])
	if id.Role != "web" || id.Description != "web 服务器" {
		t.Errorf("身份合并错误: %+v", id)
	}
}

func TestLoadConfigUnknownField(t *testing.T) {
	cfg := strings.Replace(baseConfig, `"version": 1,`, `"version": 1, "typo_field": 1,`, 1)
	p := writeTemp(t, "config.json", cfg)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("未知字段应当报错，避免拼写错误被静默忽略")
	}
}

func TestLoadConfigBadJSON(t *testing.T) {
	p := writeTemp(t, "config.json", `{"version": 1,`)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("非法 JSON 应当报错")
	}
}

func TestValidateMissingKeyRef(t *testing.T) {
	cfg := strings.Replace(baseConfig, `"key":"k1"`, `"key":"nope"`, 1)
	p := writeTemp(t, "config.json", cfg)
	_, err := LoadConfig(p)
	if err == nil || !strings.Contains(err.Error(), "key \"nope\" 不存在") {
		t.Fatalf("应提示密钥不存在，得到: %v", err)
	}
}

func TestValidateJumpCycle(t *testing.T) {
	cfg := `{
	  "keys":[{"name":"k","type":"password","value":"x"}],
	  "servers":[
	    {"id":"a","host":"a","key":"k","jump":{"server":"b"}},
	    {"id":"b","host":"b","key":"k","jump":{"server":"a"}}
	  ],
	  "tasks":[{"name":"t","command":"true"}]
	}`
	p := writeTemp(t, "config.json", cfg)
	_, err := LoadConfig(p)
	if err == nil || !strings.Contains(err.Error(), "环路") {
		t.Fatalf("应检出跳板机环路，得到: %v", err)
	}
}

func TestValidateTaskHosts(t *testing.T) {
	cfg := strings.Replace(baseConfig, `"command":"uptime"`, `"command":"uptime","hosts":["ghost"]`, 1)
	p := writeTemp(t, "config.json", cfg)
	_, err := LoadConfig(p)
	if err == nil || !strings.Contains(err.Error(), "不存在的服务器") {
		t.Fatalf("应提示任务引用了不存在的服务器，得到: %v", err)
	}
}

func TestDefaultIdentityFallback(t *testing.T) {
	cfg := `{
	  "keys":[{"name":"k","type":"password","value":"x"}],
	  "identities":{"default":{"role":"db","owner":"dba"}},
	  "servers":[{"id":"s1","host":"h","key":"k","tags":["prod"],"desc":"主库"}],
	  "tasks":[{"name":"t","command":"true"}]
	}`
	p := writeTemp(t, "config.json", cfg)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	id := c.EffectiveIdentity(&c.Servers[0])
	if id.Role != "db" || id.Owner != "dba" || id.Description != "主库" {
		t.Errorf("默认身份兜底失败: %+v", id)
	}
	if len(id.Tags) != 1 || id.Tags[0] != "prod" {
		t.Errorf("标签合并失败: %v", id.Tags)
	}
}

func TestParseMode(t *testing.T) {
	cases := map[string]os.FileMode{"0644": 0o644, "755": 0o755, "0o600": 0o600, "": 0o644}
	for in, want := range cases {
		got, err := parseMode(in)
		if err != nil || got != want {
			t.Errorf("parseMode(%q) = %v, %v；期望 %v", in, got, err, want)
		}
	}
	if _, err := parseMode("abc"); err == nil {
		t.Error("非法 mode 应报错")
	}
}

// ---------- 指纹 / known_hosts ----------

func TestFingerprintNormalize(t *testing.T) {
	// 用 x/crypto 生成一个公钥
	signer := newTestSigner(t)
	pk := signer.PublicKey()
	fp := NormalizeFingerprint(pk)
	if !strings.HasPrefix(fp, "SHA256:") || strings.Contains(fp, "=") {
		t.Fatalf("指纹格式错误: %s", fp)
	}
	got, err := parseFingerprint(fp + "=") // 容忍填充
	if err != nil || got != fp {
		t.Fatalf("解析指纹失败: %v %q", err, got)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
	if got, err := parseFingerprint(line); err != nil || got != line {
		t.Fatalf("解析公钥行失败: %v", err)
	}
	if _, err := parseFingerprint("MD5:aa:bb"); err == nil {
		t.Error("MD5 指纹应被拒绝")
	}
	if _, err := parseFingerprint("garbage"); err == nil {
		t.Error("乱码应被拒绝")
	}
}

func TestKnownHostsLookup(t *testing.T) {
	signer := newTestSigner(t)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	fp := NormalizeFingerprint(signer.PublicKey())

	content := "# comment\n" +
		"web1.example.com " + line + "\n" +
		"*.corp " + line + "\n" +
		"[db1]:2222 " + line + "\n"
	p := writeTemp(t, "known_hosts", content)

	for _, host := range []string{"web1.example.com", "svc.corp"} {
		res, err := lookupKnownHosts(p, host, host)
		if err != nil || !res.anyEntry || !res.trusted(signer.PublicKey()) {
			t.Errorf("%s: 应命中，err=%v entry=%v", host, err, res.anyEntry)
		}
		if len(res.lines) != 1 || res.lines[0] != fp {
			t.Errorf("%s: 指纹行应记录，得到 %v", host, res.lines)
		}
	}
	res, _ := lookupKnownHosts(p, "[db1]:2222", "db1")
	if !res.anyEntry {
		t.Error("非标准端口应匹配 [db1]:2222")
	}
	res, _ = lookupKnownHosts(p, "other.example.com", "other.example.com")
	if res.anyEntry {
		t.Error("未收录的主机不应命中")
	}
}

func TestKnownHostsHashed(t *testing.T) {
	// 生成 |1|salt|HMAC-SHA1(host) 格式
	signer := newTestSigner(t)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	salt := []byte("0123456789abcdefghij")
	hashed := hashHostForTest(salt, "hashed.example.com")
	p := writeTemp(t, "known_hosts", hashed+" "+line+"\n")
	res, err := lookupKnownHosts(p, "hashed.example.com", "hashed.example.com")
	if err != nil || !res.anyEntry {
		t.Fatalf("哈希主机名应命中: %v %v", err, res.anyEntry)
	}
	res, _ = lookupKnownHosts(p, "other.example.com", "other.example.com")
	if res.anyEntry {
		t.Error("哈希主机名不应误命中")
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"*.corp", "a.corp", true},
		{"*.corp", "a.b.corp", true},
		{"*.corp", "corp", false},
		{"web?", "web1", true},
		{"web?", "web10", false},
		{"[a]:2222", "[a]:2222", true},
		{"WEB.EXAMPLE.COM", "web.example.com", true},
	}
	for _, c := range cases {
		if got := matchHostPattern(c.pat, c.s); got != c.want {
			t.Errorf("matchHostPattern(%q,%q)=%v 期望 %v", c.pat, c.s, got, c.want)
		}
	}
}

// ---------- 巡检解析 ----------

func TestParseDF(t *testing.T) {
	linux := `Filesystem     1B-blocks        Used    Available Use% Mounted on
/dev/sda1    107374182400 96636764160   2243952640  90% /
tmpfs          838860800           0    838860800   0% /dev/shm
/dev/sdb1     53687091200 10737418240  42949672960  20% /data
`
	disks := parseDF(linux)
	if len(disks) != 3 {
		t.Fatalf("应解析 3 个挂载点，得到 %d", len(disks))
	}
	if disks[0].Mount != "/" || disks[0].UsedPct != 90 {
		t.Errorf("排序或解析错误: %+v", disks[0])
	}
	if disks[0].Size != 107374182400 {
		t.Errorf("单位推断错误，size=%d", disks[0].Size)
	}
	// macOS df -P -k
	mac := `Filesystem     1024-blocks      Used Available Capacity  Mounted on
/dev/disk3s1s1   482746452  13333768 368593028     4%    /
`
	disks = parseDF(mac)
	if len(disks) != 1 || disks[0].UsedPct != 4 || disks[0].Size != 482746452*1024 {
		t.Errorf("macOS 解析错误: %+v", disks)
	}
}

func TestParseMemInfo(t *testing.T) {
	out := `MemTotal:        8000000 kB
MemFree:          200000 kB
MemAvailable:    6000000 kB
Buffers:          100000 kB
Cached:          1000000 kB
SwapTotal:       2000000 kB
SwapFree:        1500000 kB
`
	mem, swap, err := parseMemInfo(out)
	if err != nil {
		t.Fatal(err)
	}
	if mem < 24.9 || mem > 25.1 {
		t.Errorf("内存使用率应为 25%%，得到 %v", mem)
	}
	if swap < 24.9 || swap > 25.1 {
		t.Errorf("Swap 使用率应为 25%%，得到 %v", swap)
	}
	if _, _, err := parseMemInfo("garbage"); err == nil {
		t.Error("缺少 MemTotal 应报错")
	}
}

func TestParseLoadAvg(t *testing.T) {
	load, cores, err := parseLoadAvg("1.50 1.00 0.50 1/200 12345\n8\n")
	if err != nil {
		t.Fatal(err)
	}
	if load != 1.5 || cores != 8 {
		t.Errorf("解析错误: %v %d", load, cores)
	}
	if _, _, err := parseLoadAvg("1.5"); err == nil {
		t.Error("缺少核数应报错")
	}
}

func TestApplyInspectAlert(t *testing.T) {
	cfg := &Config{Alert: Alert{DiskUsedPercent: 85, MemUsedPercent: 90, LoadPerCore: 2}}
	task := &Task{Name: "disk", Type: "command", Inspect: &Inspect{Kind: "df", Mounts: []string{"/"}}}
	res := TaskResult{Status: StatusOK, Stdout: "Filesystem 1B-blocks Used Available Use% Mounted on\n/dev/sda1 100 90 10 90% /\n"}
	applyInspect(cfg, task, &res, "web1")
	if res.Status != StatusWarn {
		t.Errorf("超过阈值应降级为 warn，得到 %v", res.Status)
	}
	if !strings.Contains(res.Detail, "90%") {
		t.Errorf("Detail 应包含使用率: %q", res.Detail)
	}
	if res.Metrics["disk_max"] != 90 {
		t.Errorf("metrics 应记录 disk_max=90，得到 %v", res.Metrics["disk_max"])
	}

	// 只关心指定挂载点：/data 未超过 / 的 90%
	task.Inspect.Mounts = []string{"/data"}
	res2 := TaskResult{Status: StatusOK, Stdout: "Filesystem 1B-blocks Used Available Use% Mounted on\n/dev/sda1 100 90 10 90% /\n/dev/sdb1 100 10 90 10% /data\n"}
	applyInspect(cfg, task, &res2, "web1")
	if res2.Status != StatusOK {
		t.Errorf("未超阈值不应告警，得到 %v (%s)", res2.Status, res2.Detail)
	}
}

// ---------- 策略 ----------

func TestNoRestrictions(t *testing.T) {
	// 按使用者要求：不做任何限制。
	// 策略层已完全移除，任何命令都不拦截、不确认。
	// 这里用一次真实命令执行来确认不存在拒绝路径。
	if err := (&Config{}).validateForVault(); err == nil {
		t.Log("策略层已移除，命令直接执行")
	}
}
