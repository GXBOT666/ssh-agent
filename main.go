// sshex 是一个配置驱动的 SSH 运维工具。
//
// 把"密钥、身份介绍、工作内容"都写进 JSON 配置文件，程序按配置连接
// 目标主机、执行运维任务，并输出结果或报表。
//
// 用法见 README.md，或运行: ./sshex help
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

// 构建信息，可通过 -ldflags "-X main.version=1.2.3" 注入。
var (
	version   = "1.0.0"
	buildTime = "unknown"
	commit    = "unknown"
)

const usage = `sshex —— AI 运维助手：终端对话操作 SSH 服务器

用法:
  sshex <命令> [选项]

说明:
  不带参数运行（./Ai）直接进入对话，这是推荐用法。
  第一次会问：选哪家接口、填 key、选模型，然后保存在 ~/.sshex/settings，以后不用再填。
  不需要任何配置文件。
  下面这些是"高级/批量"命令，日常用不到，直接让 AI 帮你做即可。

命令:
  chat    进入 AI 运维助手（./Ai 等价于 ./Ai chat）
  run     执行配置里的批量任务（全量 / 单主机 / 带标签筛选）
  exec    在指定主机上执行一条临时命令（不写入配置文件）
  check   体检：配置文件、密钥、服务器登录是否可用
  inv     打印配置里的密钥、服务器身份与工作内容摘要
  keys    打印配置里的密钥信息（私钥路径、指纹、类型，不打印私钥明文）
  tasks   打印工作内容（任务清单）
  hosts   打印服务器清单，包括本次筛选结果
  report  重新执行一次巡检并生成报告（等价于 run -report）
  version 打印版本
  help    打印本帮助

通用选项:
  -c, --config <path>     （仅批量命令用）配置文件路径
      --dry-run           只展示将要执行的工作，不连接主机
  -y, --yes               确认跳过策略里的高风险操作确认（用于自动化）
  -v, --verbose           打印调试日志（例如主机指纹）

run / report 选项:
  -g, --group <name>      只执行指定分组（可重复）
  -H, --host <name|id>    只执行指定主机（可重复）
      --tags <a,b>        只执行带这些标签的任务（可重复，逗号分隔）
      --report <dir>      把本次结果写成 Markdown 报告到目录
      --timeout <dur>     覆盖所有任务超时，例如 10m
      --json              结果以 JSON 输出

chat 选项:
      --reset             重新选择接口/密钥/模型

exec 选项:
  -H, --host <name|id>    目标主机（必填）
      --cmd <shell>       要执行的命令（必填）
      --timeout <dur>     超时，默认 60s
      --no-confirm        命令包含高风险关键字时不弹确认

示例:
  sshex chat                              # 进入 AI 对话（推荐）
  sshex chat "看看 web-01 的磁盘"           # 单次提问
  sshex check
  sshex inv
  sshex hosts --tags prod
  sshex run
  sshex run -g web --tags health
  sshex exec -H web-01 --cmd 'df -h'
  sshex run --report ./reports

退出码:
  0 成功   1 执行有失败   2 配置/参数错误   3 用户取消   4 高风险操作被策略拦截
`

var globalFlagsWithValue = map[string]bool{
	"-c": true, "--config": true, "-config": true, "--timeout": true, "-timeout": true,
	"-g": true, "--group": true, "-group": true,
	"-H": true, "--host": true, "-host": true,
	"--tags": true, "-tags": true, "--report": true, "-report": true, "--task": true, "-task": true,
}

var globalBoolFlags = map[string]bool{
	"-v": true, "--verbose": true, "-verbose": true,
	"-y": true, "--yes": true, "-yes": true,
	"--dry-run": true, "-dry-run": true, "--json": true, "-json": true,
	"--no-confirm": true, "-no-confirm": true, "-h": true, "--help": true, "-V": true, "--version": true,
}

// splitCommand 把参数序列拆成（子命令，子命令参数）。
// 子命令前的公共选项会被移动到子命令参数中，保证两种写法都可用。
func splitCommand(argv []string) (string, []string, error) {
	var lead []string
	i := 0
	for i < len(argv) {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			// 这是子命令
			rest := append(append([]string{}, lead...), argv[i+1:]...)
			return a, rest, nil
		}
		name := a
		if idx := strings.IndexByte(a, '='); idx > 0 {
			name = a[:idx]
			lead = append(lead, a)
			i++
			continue
		}
		if globalFlagsWithValue[name] {
			if i+1 >= len(argv) {
				return "", nil, fmt.Errorf("选项 %s 缺少参数", a)
			}
			lead = append(lead, a, argv[i+1])
			i += 2
			continue
		}
		if globalBoolFlags[name] {
			lead = append(lead, a)
			i++
			continue
		}
		return "", nil, fmt.Errorf("未知选项: %s", a)
	}
	if len(lead) > 0 {
		// 只有选项没有子命令：如果带 -h/-V 就按帮助/版本处理
		for _, a := range lead {
			if a == "-h" || a == "--help" {
				return "help", nil, nil
			}
			if a == "-V" || a == "--version" {
				return "version", nil, nil
			}
		}
	}
	return "", nil, fmt.Errorf("缺少子命令，可用: run/exec/check/inv/keys/tasks/hosts/report/version/help")
}

// isHelpOrVersion 判断参数里是否只有 -h/--help/-V/--version。
func isHelpOrVersion(args []string) bool {
	for _, a := range args {
		switch a {
		case "-h", "--help", "-V", "--version":
			return true
		}
	}
	return false
}

func main() {
	defer func() {
		// 保证终端设置、连接池等在异常退出时也得到释放。
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "内部错误: %v\n%s", r, debug.Stack())
			os.Exit(1)
		}
	}()

	// 不带参数直接进入对话（最常用）
	if len(os.Args) < 2 {
		os.Exit(cmdChat(nil))
	}

	// 允许把公共选项写在子命令前面：sshex -c x.json run 等价于 sshex run -c x.json
	cmd, args, err := splitCommand(os.Args[1:])
	if err != nil {
		// 只有选项（如 -c xxx）时也进入对话
		if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "-") && !isHelpOrVersion(os.Args[1:]) {
			os.Exit(cmdChat(os.Args[1:]))
		}
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if cmd == "" {
		os.Exit(cmdChat(nil))
	}

	var code int
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "version", "-V", "--version":
		fmt.Printf("sshex %s (commit %s, built %s)\n", version, commit, buildTime)
	case "chat":
		code = cmdChat(args)
	case "run":
		code = cmdRun(args, false)
	case "report":
		code = cmdRun(args, true)
	case "exec":
		code = cmdExec(args)
	case "check":
		code = cmdCheck(args)
	case "inv":
		code = cmdInv(args)
	case "keys":
		code = cmdKeys(args)
	case "tasks":
		code = cmdTasks(args)
	case "hosts":
		code = cmdHosts(args)
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n%s", cmd, usage)
		code = 2
	}
	os.Exit(code)
}
