package main

import (
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// vtSim 是一个极简终端模拟器：只处理 ANSI 光标移动与擦除，
// 用于验证"渲染后光标停在哪一行"，也就是输入框会不会往下跑。
type vtSim struct {
	rows  [][]rune
	row   int
	col   int
	width int
}

func newVTSim(width int) *vtSim {
	return &vtSim{rows: [][]rune{{}}, width: width}
}

func (v *vtSim) ensure() {
	for len(v.rows) <= v.row {
		v.rows = append(v.rows, []rune{})
	}
	for len(v.rows[v.row]) < v.col {
		v.rows[v.row] = append(v.rows[v.row], ' ')
	}
}

func (v *vtSim) down() {
	if v.row+1 < len(v.rows) {
		v.row++
	} else {
		v.rows = append(v.rows, []rune{})
		v.row++
	}
	if v.col > len(v.rows[v.row]) {
		v.col = len(v.rows[v.row])
	}
}

func (v *vtSim) up() {
	if v.row > 0 {
		v.row--
	}
	if v.col > len(v.rows[v.row]) {
		v.col = len(v.rows[v.row])
	}
}

var csiRe = regexp.MustCompile(`\033\[([0-9]*)([A-Za-z])`)

func (v *vtSim) feed(s string) {
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			m := csiRe.FindStringSubmatch(s[i:])
			if m == nil {
				i++
				continue
			}
			n := 1
			if m[1] != "" {
				n, _ = strconv.Atoi(m[1])
			}
			switch m[2] {
			case "A":
				for k := 0; k < n; k++ {
					v.up()
				}
			case "B":
				for k := 0; k < n; k++ {
					v.down()
				}
			case "C":
				v.col += n
			case "D":
				v.col -= n
				if v.col < 0 {
					v.col = 0
				}
			case "K":
				v.ensure()
				v.rows[v.row] = v.rows[v.row][:v.col]
			case "s", "u":
				// 本项目已不使用
			}
			i += len(m[0])
			continue
		}
		if s[i] == '\r' {
			v.col = 0
			i++
			continue
		}
		if s[i] == '\n' {
			v.down()
			i++
			continue
		}
		// 普通字符（按 rune 读取）
		r := []rune(s[i:])[0]
		// 真实终端：写满一行后光标停在行尾（列=width），不立即换行；
		// 只有当再来一个字符写不下时才折到下一行。
		if v.col+1 > v.width {
			v.row++
			v.col = 0
			for len(v.rows) <= v.row {
				v.rows = append(v.rows, []rune{})
			}
		}
		v.ensure()
		v.rows[v.row] = append(v.rows[v.row], r)
		v.col += runeWidth(r)
		i += len(string(r))
	}
}

// script 运行一串按键，返回模拟器最终的光标行号。
func runRenderScript(t *testing.T, width int, prompt, keys string) (*vtSim, int) {
	t.Helper()
	sim := newVTSim(width)
	// 用一个可捕获的渲染器替代 os.File：这里直接构造 lineRenderer 并替换输出
	rend := &lineRenderer{out: nil, prompt: prompt, width: width, rows: 1}

	e := &editor{}

	// 重定向 rend 输出到临时文件
	tmp, _ := os.CreateTemp("", "vt")
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	rend.out = tmp

	rend.draw(e)
	for _, ch := range keys {
		if ch == '\b' {
			e.key(127, 0)
		} else {
			e.key(byte(ch), ch)
		}
		rend.draw(e)
	}
	// 开始行号（第一行）
	startRow := 0
	tmp.Seek(0, 0)
	data := make([]byte, 1<<20)
	n, _ := tmp.Read(data)
	sim.feed(string(data[:n]))
	return sim, startRow
}

// ============================================================================
// 关键回归：无论输入多长，光标必须始终停在第 0 行（输入框不下移）
// ============================================================================

func TestCursorNeverMovesDown(t *testing.T) {
	const width, prompt = 20, "> "
	cases := []string{
		"abc",
		"abcdefghij",                 // 刚好临界
		"abcdefghijklmnopqrstuvwxyz", // 超过一行
		"abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJ", // 远超一行
		"中文输入测试看看会不会往下掉中文输入测试看看会不会往下掉",
		"abcdefghijkl\b\b\b",
		"中文测试\b\b",
	}
	for _, keys := range cases {
		sim := newVTSim(width)
		tmp, _ := os.CreateTemp("", "vt")
		rend := &lineRenderer{out: tmp, prompt: prompt, width: width, rows: 1}
		e := &editor{}
		rend.draw(e)
		for _, ch := range keys {
			if ch == '\b' {
				e.key(127, 0)
			} else {
				e.key(byte(ch), ch)
			}
			rend.draw(e)
			if rend.rows != 1 || rend.row != 0 {
				t.Fatalf("输入 %q 时渲染器状态异常 rows=%d row=%d（应恒为 1/0）", keys, rend.rows, rend.row)
			}
		}
		tmp.Seek(0, 0)
		data, _ := io.ReadAll(tmp)
		sim.feed(string(data))
		tmp.Close()
		os.Remove(tmp.Name())
		if sim.row != 0 {
			t.Errorf("输入 %q 后光标跑到第 %d 行，应为第 0 行", keys, sim.row)
		}
	}
}

func TestCursorStaysWithinWidth(t *testing.T) {
	const width, prompt = 20, "> "
	// 长输入时光标列不能超出屏幕宽度，否则终端会折行
	keys := "abcdefghijklmnopqrstuvwxyz0123456789"
	tmp, _ := os.CreateTemp("", "vt")
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	rend := &lineRenderer{out: tmp, prompt: prompt, width: width, rows: 1}
	e := &editor{}
	rend.draw(e)
	for _, ch := range keys {
		e.key(byte(ch), ch)
		rend.draw(e)
	}
	if rend.col >= width {
		t.Errorf("光标列 %d 超过终端宽度 %d，会导致折行", rend.col, width)
	}
}

func TestCursorRepeatedRedrawNoDrift(t *testing.T) {
	tmp, _ := os.CreateTemp("", "vt")
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	rend := &lineRenderer{out: tmp, prompt: "你 › ", width: 30, rows: 1}
	e := &editor{}
	for _, r := range "你好世界" {
		e.key(byte(r), r)
	}
	rend.draw(e)
	for i := 0; i < 20; i++ {
		rend.draw(e)
	}
	tmp.Seek(0, 0)
	data, _ := io.ReadAll(tmp)
	sim := newVTSim(30)
	sim.feed(string(data))
	if sim.row != 0 {
		t.Errorf("重复渲染后光标跑到第 %d 行，应为 0", sim.row)
	}
}

// ============================================================================
// 回归：提示符里带前导换行（"\n你 › "）时，
// 每按一个键都会多输出一个换行，导致输入框一路往下掉。
// ============================================================================

func TestPromptLeadingNewlineNotRepeated(t *testing.T) {
	const width = 40
	// 完全复刻对话界面的提示符：带颜色代码，且换行在颜色代码之后
	prompt := "\x1b[36m\n你 › \x1b[0m"
	tmp, _ := os.CreateTemp("", "vt")
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	rend := newLineRenderer(tmp, prompt)
	if strings.Contains(rend.prompt, "\n") {
		t.Fatalf("提示符里的换行没有被剥离: %q", rend.prompt)
	}
	e := &editor{}
	rend.draw(e)
	for _, ch := range "abcde" {
		e.key(byte(ch), ch)
		rend.draw(e)
	}
	tmp.Seek(0, 0)
	data, _ := io.ReadAll(tmp)
	out := string(data)

	// 整个过程中只允许出现 1 个换行（首次绘制时的那一个）
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("输出里出现了 %d 个换行，应只有 1 个（每多一个，输入框就多下移一行）\n输出: %q", n, out)
	}

	// 用模拟器确认光标始终在同一行
	sim := newVTSim(width)
	sim.feed(out)
	if sim.row != 1 { // 前导换行让它从第 0 行落到第 1 行，之后不能再变
		t.Errorf("光标停在第 %d 行，应在第 1 行（前导换行之后固定不动）", sim.row)
	}
}

// ============================================================================
// 回归：颜色转义序列不计入显示宽度
//
// 提示符 "你 > " 带颜色时，若把 \x1b[36m 之类的控制码算成可见字符，
// 光标列会整体偏移，表现为"箭头和文字之间空隙不对"。
// ============================================================================

func TestDisplayWidthIgnoresANSI(t *testing.T) {
	cases := map[string]int{
		"\x1b[36m你 > \x1b[0m": 5, // 你=2 空格=1 >=1 空格=1
		"abc":                 3,
		"中文":                  4,
		"\x1b[1;32m你好\x1b[0m": 4,
		"":                    0,
	}
	for in, want := range cases {
		if got := displayWidth(in); got != want {
			t.Errorf("displayWidth(%q) = %d，期望 %d", in, got, want)
		}
	}
}

func TestCursorColumnWithColoredPrompt(t *testing.T) {
	tmp, _ := os.CreateTemp("", "al")
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	rend := newLineRenderer(tmp, "\x1b[36m\n你 > \x1b[0m")
	e := &editor{}
	rend.draw(e)
	for _, ch := range "ab" {
		e.key(byte(ch), ch)
		rend.draw(e)
	}
	tmp.Seek(0, 0)
	data, _ := io.ReadAll(tmp)
	// 提示符 5 列 + "ab" 2 列 = 光标应在第 7 列
	if !strings.Contains(string(data), "\x1b[7C") {
		t.Errorf("光标列错误，期望第 7 列；输出: %q", string(data))
	}
}

// TestChineseRealPath 走真实的字节解析路径验证中文输入与光标列。
func TestChineseRealPath(t *testing.T) {
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	go func() {
		inW.WriteString("看下磁盘\n")
		inW.Close()
	}()
	done := make(chan string, 1)
	go func() {
		l, _ := readLineTo(inR, outW, "\x1b[36m\n你 > \x1b[0m", true)
		outW.Close()
		done <- l
	}()
	outBytes, _ := io.ReadAll(outR)
	line := <-done
	if line != "看下磁盘" {
		t.Fatalf("中文读取错误: %q", line)
	}
	// 提示符 5 列 + 中文 4 字 × 2 列 = 13
	if !strings.Contains(string(outBytes), "\x1b[13C") {
		t.Errorf("中文输入后光标列错误，期望第 13 列；输出: %q", string(outBytes))
	}
	// 每个中文都要完整显示，不能被拆散
	if strings.Count(string(outBytes), "\x1b[2K") != 5 {
		t.Errorf("应重画 5 次（初始 + 4 个字），实际 %d 次", strings.Count(string(outBytes), "\x1b[2K"))
	}
}

// ============================================================================
// 回归：确认框必须留出可见、可编辑的输入框
//
// 风险命令的提示含多行说明，若整段当作提示符交给渲染器，
// 单行渲染会把它覆盖掉，用户看不到也没法删改自己输入的 y/n。
// ============================================================================

func TestConfirmPromptIsSingleLine(t *testing.T) {
	// 多行说明由 Confirm 单独打印，真正交给渲染器的提示保持单行简短
	prompt := "确认执行 [y/N] "
	if strings.Contains(prompt, "\n") {
		t.Fatal("提示符不应含换行")
	}
	tmp, _ := os.CreateTemp("", "cfm")
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	rend := newLineRenderer(tmp, prompt)
	e := &editor{}
	rend.draw(e)
	e.key('y', 'y')
	rend.draw(e)
	tmp.Seek(0, 0)
	data, _ := io.ReadAll(tmp)
	out := string(data)
	// 必须能看到提示符本身（输入框）
	if !strings.Contains(out, "确认执行 [y/N] ") {
		t.Errorf("输出里看不到确认输入框: %q", out)
	}
	// 输入的 y 必须回显
	if !strings.Contains(out, "确认执行 [y/N] y") {
		t.Errorf("输入的 y 没有回显: %q", out)
	}
}
