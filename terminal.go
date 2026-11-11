package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/term"
)

// isTerminal 判断标准输入/输出是否为终端。
func isTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// isOutputTerminal 判断 stdout 是否终端（用于决定是否上色）。
func isOutputTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// readSecretLine 读取密码：不回显，支持退格。
// 自己逐字节读，不用 term.ReadPassword——后者会清空输入缓冲，
// 与外面 bufio 预读的数据冲突，正是"输入错乱、删不掉"的另一半原因。
func readSecretLine(in *os.File, prompt string) (string, error) {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		return readLinePlain(in, prompt)
	}
	fmt.Print(prompt)
	old, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer term.Restore(fd, old)

	var line []rune
	buf := make([]byte, 64)
	for {
		n, err := in.Read(buf)
		if err != nil {
			return string(line), err
		}
		for i := 0; i < n; i++ {
			b := buf[i]
			switch b {
			case '\r', '\n':
				fmt.Print("\r\n")
				return string(line), nil
			case 3: // Ctrl-C
				fmt.Print("^C\r\n")
				line = nil
				return "", ErrCanceled
			case 4: // Ctrl-D
				if len(line) == 0 {
					fmt.Print("\r\n")
					return "", errEOF
				}
			case 127, 8:
				if len(line) > 0 {
					line = line[:len(line)-1]
				}
			default:
				if b < 0x20 {
					continue // 忽略方向键等控制字符（密码不需要编辑）
				}
				if r, size := decodeRune(buf[i:n]); r > 0 {
					i += size - 1
					line = append(line, r)
				}
			}
		}
	}
}

// promptLine 读取一行普通输入（带行编辑：退格、方向键、Home/End、Ctrl-U/W）。
//
// 这里是"文字删不掉"的根源所在：旧实现自己切到 raw 模式后逐字节读取，
// 只处理了 127/8 两个退格码，其它按键直接当字符塞进结果，
// 而 raw 模式又关闭了终端的行编辑与回显，于是出现删不掉、字符乱跑的现象。
func promptLine(prompt string) (string, error) {
	return readLine(os.Stdin, prompt)
}

// readLine 在 raw 模式下读取一行，自行处理编辑键。
// 非终端（管道/重定向）时退化为普通行读取。
func readLine(in *os.File, prompt string) (string, error) {
	return readLineTo(in, os.Stdout, prompt, isTerminal())
}

// readLineTo 读入一行。tty 为 true 时采用原始模式并提供行编辑与主动回显。
// 把 tty 作为参数是为了能在测试里不依赖真实终端地验证回显逻辑。
func readLineTo(in *os.File, out *os.File, prompt string, tty bool) (string, error) {
	fd := int(in.Fd())
	if !tty {
		return readLinePlain(in, prompt)
	}

	// 只有真正的终端才切原始模式；非终端 fd 上 MakeRaw 会失败，
	// 此时照样按"逐键读取 + 主动回显"处理（测试就走这条路径）。
	if term.IsTerminal(fd) {
		old, err := term.MakeRaw(fd)
		if err != nil {
			return "", err
		}
		// 一定要恢复终端，否则退出后 shell 会处于怪异状态
		defer term.Restore(fd, old)
	}

	var (
		buf = make([]byte, 64)
		esc []byte // 处理中的 ANSI 序列
	)
	ed := &editor{}
	rend := newLineRenderer(out, prompt)
	// 调试：设置 SSHEX_DEBUG_INPUT=1 时，把每轮渲染的转义序列写到文件，便于定位光标问题
	if dbg := os.Getenv("SSHEX_DEBUG_INPUT"); dbg != "" {
		if f, err := os.OpenFile(dbg, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			fmt.Fprintf(f, "\n--- terminal width=%d prompt=%q ---\n", rend.width, prompt)
			rend.debugFile = f
		}
	}
	rend.draw(ed) // 先画出提示符
	redraw := func() { rend.draw(ed) }

	for {
		n, err := in.Read(buf)
		if err != nil {
			return string(ed.line), err
		}
		for i := 0; i < n; i++ {
			b := buf[i]

			// 处理 ANSI 转义序列（方向键等）：ESC [ X
			if len(esc) > 0 || b == 0x1b {
				esc = append(esc, b)
				done, seq := parseEscape(esc)
				if !done {
					continue
				}
				esc = nil
				ed.escape(seq)
				redraw()
				continue
			}

			if b == '\r' || b == '\n' {
				rend.finish()
				return string(ed.line), nil
			}
			if b == 3 { // Ctrl-C
				rend.finish()
				return "", ErrCanceled
			}
			if b == 4 { // Ctrl-D
				if len(ed.line) == 0 {
					rend.finish()
					return "", errEOF
				}
				continue
			}

			var r rune
			size := 1
			if b >= 0x20 {
				r, size = decodeRune(buf[i:n])
			}
			if size > 1 {
				i += size - 1
			}
			ed.key(b, r)
			redraw()
		}
	}
}

// lineRenderer 负责把编辑中的一行"原地"重画到终端上。
//
// 难点：内容超过终端宽度会折成多个物理行，简单用 \r 回行首
// 只会停在折行后的那一行，光标位置就乱了——表现就是"输入一个字
// 或删一个字，输入框往下掉一行"。
//
// 办法：记录上次渲染占用的物理行数与光标位置，每次重画时
// 相对移动回起点、清行、重新绘制，再把光标定位到编辑处。
type lineRenderer struct {
	out    *os.File
	prompt string
	width  int
	rows   int    // 上次渲染占用的物理行数
	row    int    // 上次光标所在物理行
	col    int    // 上次光标所在列
	lead   string // 首次绘制前输出的前导换行

	debugFile *os.File
	drew      bool
}

func newLineRenderer(out *os.File, prompt string) *lineRenderer {
	// 提示符里可能带换行（例如带颜色的 "\x1b[36m\n你 › \x1b[0m"），
	// 用于在两次对话之间空一行。这类换行只能在整个输入开始前输出一次；
	// 若每次重画都原样打印，每按一个键就会多出一个换行，输入框一路往下掉。
	//
	// 注意：换行不一定在开头——前面可能还有颜色代码，所以要在整串里找。
	clean, lead := stripNewlines(prompt)
	return &lineRenderer{
		out: out, prompt: clean, lead: lead,
		width: terminalWidth(out), rows: 1,
	}
}

// stripNewlines 把字符串里的换行移除，返回清理后的字符串与"前导换行"串。
// 前导换行串用于在首次绘制前输出，从而保留"空一行"的视觉效果。
func stripNewlines(s string) (clean, lead string) {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' {
			lead += "\r\n"
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), lead
}

// terminalWidth 取终端宽度，失败时按 80 估算。
func terminalWidth(f *os.File) int {
	if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
		return w
	}
	return 80
}

// displayWidth 估算显示宽度：中日韩字符与 emoji 占 2 列。
// ANSI 转义序列（颜色等）不占宽度，必须先剔除，否则光标位置会整体偏移。
func displayWidth(s string) int {
	return visibleWidth(stripANSI(s))
}

// 匹配 CSI 序列，例如 \x1b[36m、\x1b[0m、\x1b[2K
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

// stripANSI 去掉 ANSI 转义序列。
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return ansiRe.ReplaceAllString(s, "")
}

// visibleWidth 计算纯文本的显示宽度。
func visibleWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20:
		return 0
	case r >= 0x1100 && (r <= 0x115F ||
		r == 0x2329 || r == 0x232A ||
		(r >= 0x2E80 && r <= 0xA4CF && r != 0x303F) ||
		(r >= 0xAC00 && r <= 0xD7A3) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFE30 && r <= 0xFE6F) ||
		(r >= 0xFF00 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6) ||
		(r >= 0x1F300 && r <= 0x1FAFF) ||
		(r >= 0x20000 && r <= 0x3FFFD)):
		return 2
	}
	return 1
}

// posOf 计算第 idx 个字符之后的 (行, 列)。
func (r *lineRenderer) posOf(prefix string) (row, col int) {
	w := displayWidth(prefix)
	return w / r.width, w % r.width
}

// draw 重画输入行。
//
// 设计要点：输入**始终只占一个物理行**。内容超过终端宽度时水平滚动，
// 只显示光标附近的一段。这样完全不存在折行，也就不需要计算占用了
// 几个物理行——从根本上避免"每按一个键输入框往下掉一行"。
//
// 每轮重画：回到本行行首 → 清整行 → 写可见内容 → 把光标移到正确列。
func (r *lineRenderer) draw(e *editor) {
	promptW := displayWidth(r.prompt)
	avail := r.width - promptW - 1 // 留 1 列，避免恰好写满触发折行
	if avail < 8 {
		avail = 8
	}

	chars := []rune(e.line)
	// 光标左侧的显示宽度
	curW := 0
	for _, ch := range chars[:e.pos] {
		curW += runeWidth(ch)
	}

	// 可见窗口起点：光标超出右边界时右移，保证光标可见
	start := 0
	if curW > avail {
		w, i := 0, e.pos
		for i > 0 {
			nw := w + runeWidth(chars[i-1])
			if nw > avail-1 {
				break
			}
			w = nw
			i--
		}
		start = i
		curW = w
	}

	// 收集可见字符（不超过 avail 列）
	var shown strings.Builder
	shownW := 0
	for _, ch := range chars[start:] {
		w := runeWidth(ch)
		if shownW+w > avail {
			break
		}
		shown.WriteRune(ch)
		shownW += w
	}

	var b strings.Builder
	b.WriteString("\r\033[2K") // 回到行首并清整行
	b.WriteString(r.prompt)
	b.WriteString(shown.String())
	b.WriteString("\r")
	if col := promptW + curW; col > 0 {
		fmt.Fprintf(&b, "\033[%dC", col)
	}

	r.rows, r.row, r.col = 1, 0, promptW+curW
	out := b.String()
	if !r.drew {
		// 前导换行只在第一次绘制时输出
		out = r.lead + out
		r.drew = true
	}
	if r.debugFile != nil {
		fmt.Fprintf(r.debugFile, "pos=%d line=%q => %q\n", e.pos, string(e.line), out)
	}
	fmt.Fprint(r.out, out)
}

// finish 输出换行，并把状态重置为起点。
func (r *lineRenderer) finish() {
	fmt.Fprint(r.out, "\r\n")
	r.rows, r.row, r.col = 1, 0, 0
}

// errEOF 表示 Ctrl-D 或输入结束。
var errEOF = errors.New("eof")

// ErrCanceled 表示用户按了 Ctrl-C。
var ErrCanceled = errors.New("已取消")

// editor 是行缓冲编辑器：只管字符与光标，不碰终端。
// 这样编辑逻辑可以脱离终端单独测试。
type editor struct {
	line []rune
	pos  int
}

// key 处理一个普通字符键或控制键。
func (e *editor) key(ch byte, r rune) {
	switch ch {
	case 127, 8: // Backspace
		if e.pos > 0 {
			e.line = append(e.line[:e.pos-1], e.line[e.pos:]...)
			e.pos--
		}
	case 21: // Ctrl-U
		e.line = e.line[:0]
		e.pos = 0
	case 23: // Ctrl-W
		for e.pos > 0 && unicode.IsSpace(e.line[e.pos-1]) {
			e.line = append(e.line[:e.pos-1], e.line[e.pos:]...)
			e.pos--
		}
		for e.pos > 0 && !unicode.IsSpace(e.line[e.pos-1]) {
			e.line = append(e.line[:e.pos-1], e.line[e.pos:]...)
			e.pos--
		}
	case 11: // Ctrl-K
		e.line = e.line[:e.pos]
	default:
		if r == 0 || ch < 0x20 {
			return
		}
		tail := append([]rune{r}, e.line[e.pos:]...)
		e.line = append(e.line[:e.pos], tail...)
		e.pos++
	}
}

// escape 处理方向键等转义序列（seq 形如 "D"、"C"、"3~"）。
func (e *editor) escape(seq string) {
	switch seq {
	case "D", "A", "B": // 左
		if e.pos > 0 {
			e.pos--
		}
	case "C": // 右
		if e.pos < len(e.line) {
			e.pos++
		}
	case "H", "1~", "7~": // Home
		e.pos = 0
	case "F", "4~", "8~": // End
		e.pos = len(e.line)
	case "3~": // Delete
		if e.pos < len(e.line) {
			e.line = append(e.line[:e.pos], e.line[e.pos+1:]...)
		}
	}
}

// String 返回当前内容。
func (e *editor) String() string { return string(e.line) }

// parseEscape 判断转义序列是否完整，返回末尾字母/数字~作为动作标识。
func parseEscape(seq []byte) (bool, string) {
	if len(seq) < 2 {
		return false, ""
	}
	switch seq[1] {
	case '[':
		for i := 2; i < len(seq); i++ {
			c := seq[i]
			if (c >= 'A' && c <= 'Z') || c == '~' {
				return true, string(seq[2 : i+1])
			}
		}
		return false, ""
	case 'O': // ESC O A/B/C/D
		if len(seq) >= 3 {
			return true, string(seq[2])
		}
		return false, ""
	default:
		return true, ""
	}
}

// decodeRune 从字节切片头部解出一个 rune（返回 0 表示字节不完整，交给下一轮）。
func decodeRune(buf []byte) (rune, int) {
	if len(buf) == 0 {
		return 0, 0
	}
	b := buf[0]
	switch {
	case b < 0x80:
		return rune(b), 1
	case b&0xE0 == 0xC0:
		if len(buf) >= 2 {
			return rune(b&0x1F)<<6 | rune(buf[1]&0x3F), 2
		}
	case b&0xF0 == 0xE0:
		if len(buf) >= 3 {
			return rune(b&0x0F)<<12 | rune(buf[1]&0x3F)<<6 | rune(buf[2]&0x3F), 3
		}
	case b&0xF8 == 0xF0:
		if len(buf) >= 4 {
			return rune(b&0x07)<<18 | rune(buf[1]&0x3F)<<12 | rune(buf[2]&0x3F)<<6 | rune(buf[3]&0x3F), 4
		}
	}
	return 0, 0
}

// readLinePlain 非终端环境的行读取。
func readLinePlain(in *os.File, prompt string) (string, error) {
	fmt.Print(prompt)
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n == 0 || err != nil {
			if sb.Len() == 0 {
				return "", errEOF
			}
			return strings.TrimRight(sb.String(), "\r\n"), nil
		}
		if buf[0] == '\n' {
			return strings.TrimRight(sb.String(), "\r"), nil
		}
		sb.WriteByte(buf[0])
	}
}
