package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// typeKeys 模拟用户按键序列（\b 表示退格），返回最终内容。
func typeKeys(seq string) string {
	e := &editor{}
	for _, r := range seq {
		if r == '\b' {
			e.key(127, 0)
			continue
		}
		e.key(byte(r), r)
	}
	return e.String()
}

func TestEditorBackspace(t *testing.T) {
	cases := map[string]string{
		"abc\b":      "ab",
		"abc\b\b\b":  "",
		"\b\b":       "",
		"中文\b":       "中", // 中文按字符退格，不是按字节
		"hello\b\b!": "hel!",
	}
	for in, want := range cases {
		if got := typeKeys(in); got != want {
			t.Errorf("typeKeys(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestEditorCursor(t *testing.T) {
	e := &editor{}
	for _, r := range "ac" {
		e.key(byte(r), r)
	}
	e.escape("D")
	e.key('b', 'b')
	if got := e.String(); got != "abc" {
		t.Errorf("插入后 = %q", got)
	}
	e.escape("3~")
	if got := e.String(); got != "ab" {
		t.Errorf("Delete 后 = %q", got)
	}
	e.escape("H")
	e.key(127, 0)
	if got := e.String(); got != "ab" {
		t.Errorf("行首退格后 = %q", got)
	}
	e.escape("F")
	e.key('c', 'c')
	if got := e.String(); got != "abc" {
		t.Errorf("End 后追加 = %q", got)
	}
}

func TestEditorWordAndLineOps(t *testing.T) {
	e := &editor{}
	for _, r := range "docker restart nginx" {
		e.key(byte(r), r)
	}
	e.key(23, 0)
	if got := e.String(); got != "docker restart " {
		t.Errorf("Ctrl-W 后 = %q", got)
	}
	e.key(23, 0)
	if got := e.String(); got != "docker " {
		t.Errorf("再 Ctrl-W 后 = %q", got)
	}
	e.key(21, 0)
	if got := e.String(); got != "" {
		t.Errorf("Ctrl-U 后 = %q", got)
	}
}

func TestEditorIgnoresControlChars(t *testing.T) {
	e := &editor{}
	e.key(1, 0)
	e.key(2, 0)
	e.key('x', 'x')
	if got := e.String(); got != "x" {
		t.Errorf("控制字符被插入了: %q", got)
	}
}

// ============================================================================
// 回显回归测试
//
// "输入不了文字"的直接原因：raw 模式关闭了终端自身回显，
// 每按一个键都必须主动重画整行。这里直接调用 readLineTo 并捕获它的输出，
// 确保测试真的覆盖到生产代码里的 redraw 调用。
// ============================================================================

func runLine(t *testing.T, keys string) (string, string) {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		inW.WriteString(keys)
		inW.Close()
	}()

	type res struct {
		line string
		err  error
	}
	done := make(chan res, 1)
	go func() {
		l, e := readLineTo(inR, outW, "> ", true)
		outW.Close()
		done <- res{l, e}
	}()

	outBytes, _ := io.ReadAll(outR)
	r := <-done
	if r.err != nil && !errors.Is(r.err, errEOF) && !errors.Is(r.err, ErrCanceled) {
		t.Logf("readLineTo 返回错误: %v（管道 EOF 属正常）", r.err)
	}
	return r.line, string(outBytes)
}

func TestReadLineEchoesEveryKey(t *testing.T) {
	line, out := runLine(t, "abc\r")
	if line != "abc" {
		t.Fatalf("读到的内容 = %q，期望 abc", line)
	}
	// 每打一个字符都必须出现一次重画，否则用户看不到自己输入的字
	for _, want := range []string{"> a", "> ab", "> abc"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里缺少回显 %q\n实际输出: %q", want, out)
		}
	}
}

func TestReadLineEchoChinese(t *testing.T) {
	line, out := runLine(t, "磁盘\r")
	if line != "磁盘" {
		t.Fatalf("中文读入错误: %q", line)
	}
	if !strings.Contains(out, "> 磁盘") {
		t.Errorf("中文未回显，输出: %q", out)
	}
}

func TestReadLineBackspaceEcho(t *testing.T) {
	line, out := runLine(t, "ab\x7f\r")
	if line != "a" {
		t.Fatalf("退格后内容 = %q，期望 a", line)
	}
	// 最后一次重画不能还留着被删掉的 b（新渲染器用 \033[2K 清整行）
	idx := strings.LastIndex(out, "\r\033[2K")
	if idx < 0 {
		t.Fatalf("没有发生重画，输出: %q", out)
	}
	if last := out[idx:]; !strings.Contains(last, "> a") || strings.Contains(last, "> ab") {
		t.Errorf("退格后应重画为 \"> a\"，实际: %q", last)
	}
}

func TestReadLineCancel(t *testing.T) {
	_, err := func() (string, error) {
		inR, inW, _ := os.Pipe()
		outR, outW, _ := os.Pipe()
		go func() { inW.WriteString("abc\x03"); inW.Close() }()
		go io.Copy(io.Discard, outR)
		l, e := readLineTo(inR, outW, "> ", true)
		outW.Close()
		return l, e
	}()
	if !errors.Is(err, ErrCanceled) {
		t.Errorf("Ctrl-C 应返回 ErrCanceled，得到 %v", err)
	}
}
