# sshex —— AI 运维助手（终端对话式 SSH 运维）

在终端里用中文告诉 AI 你要做什么，它自己登录服务器、执行命令、给你结论，
并把结论写进记忆文件，下次还能记得这台机器是干什么的、出过什么问题。

```
你 › 看看 web-01 磁盘还剩多少
  $ web-01: df -h /
  📝 已写入记忆: web-01 磁盘 / 使用率 91%，主要占用在 /var/log
AI › web-01 的 / 分区使用率 91%，已超过 85% 的告警线，建议清理 /var/log。
```

四个部分组成：

| 部分 | 存在哪 | 说明 |
|---|---|---|
| **模型接口** | `~/.sshex/settings` | 兼容 OpenAI 协议的任意模型（DeepSeek / OpenAI / 通义千问 / Kimi / 智谱 / Ollama / vLLM） |
| **服务器账号密码** | `~/.sshex/servers/<IP>.json`，每台机器一个文件 | 在终端里输入，**不经过 AI**；文件权限 0600，**未加密** |
| **长期记忆** | `data/MEMORY.md` + 每台机器的 `last_summary`/`notes` | AI 自动总结运维结论并写入，人类可直接编辑 |
| **安全策略** | 内置默认 | 灾难命令硬拦截、风险命令确认、审计日志 |

## 目录

- [快速开始](#快速开始)
- [终端对话](#终端对话)
- [模型配置](#模型配置)
- [服务器录入](#服务器录入)
- [记忆文件](#记忆文件)
- [安全设计](#安全设计)
- [批量模式（原 sshex 功能）](#批量模式)
- [常见问题](#常见问题)

---

## 快速开始

```bash
go build -o Ai .     # 需要 Go 1.26+；国内先 export GOPROXY=https://goproxy.cn,direct
./Ai
```

**不需要任何配置文件。** 第一次运行时在对话框里问三件事：

```
1 DeepSeek
2 OpenAI
3 通义千问
4 Kimi
5 本地 Ollama
> 1
https://api.deepseek.com >
key > ****
1 deepseek-flash
2 deepseek-v4-pro
> 1

sshex  deepseek-flash  0 台
```

- 模型名不用背——程序会向接口查询**实际可用**的模型再让你选；
- 选完后保存在 `~/.sshex/settings`，**以后每次启动直接进对话，不用再填**；
- 想换模型/换 key：`./Ai --reset`，或在对话里输入 `/model`。

然后直接说话：

```
你 › 帮我添加一台服务器 10.0.1.11，root，生产的 web 机器
  10.0.1.11:22
  pass > ****
AI › 已添加并连接成功

你 › 看看磁盘还剩多少
  ... $ df -h
  记 磁盘使用率 4%
AI › / 分区用了 12G，还剩 448G……
```

## 终端对话

```bash
./Ai                             # 进入对话（不带参数即可）
./Ai chat "看看 web-01 的磁盘"    # 单次提问后退出
```

对话中可用的指令：

| 指令 | 作用 |
|---|---|
| `/help` | 帮助 |
| `/servers` | 列出已录入的服务器（含用途、最近结论） |
| `/add` | 交互式录入服务器 |
| `/del <ID>` | 删除服务器（连同文件） |
| `/edit <ID>` | 修改用途/负责人/备注 |
| `/connect <ID>` | 测试连接某台服务器 |
| `/memory` | 查看记忆文件位置与最近内容 |
| `/remember <内容>` | 手动写一条记忆 |
| `/reset` | 清空当前对话上下文（记忆保留） |
| `/quit` | 退出 |

AI 能用的工具（由程序提供，AI 只能通过它们动手）：

- `list_servers` 查看服务器清单
- `run_on_server` 在指定机器执行命令（**所有命令都过安全策略**）
- `upload_file` 上传文件（下发配置/脚本，可顺带执行命令）
- `download_file` 下载文件（拉日志/配置来分析）
- `add_server` 新增服务器（**密码由终端输入，不经过模型**）
- `remove_server` 删除服务器记录（会先问你确认）
- `remember` 写入长期记忆

你不需要记住任何命令——直接说"把 app.conf 传到 web-01 的 /etc/nginx/conf.d/ 然后 nginx -t"，
AI 会自己选工具完成。

---

## 模型配置

```json
"model": {
  "name": "DeepSeek",
  "base_url": "https://api.deepseek.com",
  "api_key": "${DEEPSEEK_API_KEY}",
  "model": "deepseek-flash",
  "timeout_sec": 120,
  "temperature": 0.2
}
```

常见厂商的 `base_url` 与 `model`：

| 厂商 | base_url | model 示例 |
|---|---|---|
| DeepSeek | `https://api.deepseek.com` | `deepseek-flash` / `deepseek-v4-pro` |
| OpenAI | `https://api.openai.com/v1` | `gpt-4o-mini` / `gpt-4o` |
| 通义千问 | `https://dashscope.aliyuncs.com/compatible-mode/v1` | `qwen-plus` / `qwen-max` |
| Kimi | `https://api.moonshot.cn/v1` | `moonshot-v1-8k` |
| 智谱 GLM | `https://open.bigmodel.cn/api/paas/v4` | `glm-4-plus` |
| 本地 Ollama | `http://127.0.0.1:11434/v1` | `qwen2.5` / `deepseek-r1` |
| vLLM / LM Studio | `http://127.0.0.1:8000/v1` | 你加载的模型名 |

说明：

- `base_url` 写到 `/v1` 或只写域名都行，程序会自动补全 `/chat/completions`；
- 本地接口（localhost / 127.0.0.1）允许不填 `api_key`；
- `api_key` 支持 `${环境变量}`，启动时会检查变量是否存在；
- 想换模型只改这一段，其它都不用动；
- `system_prompt` 可自定义 AI 的性格与规则（留空用内置的运维提示词）。

---

## 服务器录入

两种方式，效果一样：

**方式一：对话中让 AI 加**（推荐）——你描述，AI 调 `add_server`，终端提示你输入密码。

**方式二：`/add` 交互式录入**——逐项提示 IP、端口、账号、用途、负责人、标签、密码。

每台服务器单独一个文件 `data/servers/<ID>.json`：

```json
{
  "id": "web-01",
  "ip": "10.0.1.11",
  "port": 22,
  "username": "root",
  "password": "password-here",
  "purpose": "生产 web，跑 Nginx",
  "owner": "前端组 · 张三",
  "tags": ["prod", "nginx"],
  "notes": ["重启前先执行 nginx -t"],
  "last_summary": "web-01 磁盘 / 使用率 91%",
  "last_used_at": "2026-11-11T09:23:03+08:00"
}
```

- 文件权限 **0600**，但**内容未加密**——请勿放到同步盘或提交到 git（建议把 `data/` 加进 `.gitignore`）；
- 留空密码则改用私钥路径或 ssh-agent；
- 同一 IP 不同端口是两台不同的机器，可用 `10.0.0.1:2222` 精确指定。

---

## 记忆文件

三层记忆，都可以直接用文本编辑器改：

| 文件 | 内容 |
|---|---|
| `data/MEMORY.md` | 全局运维记忆，按时间追加：结论、关键数据、执行过的命令 |
| `data/servers/<ID>.json` | 机器级记忆：用途、负责人、备注、最近一次结论 |
| `data/history.jsonl` | 原始流水，一行一条 JSON，便于检索 |

实际路径是 `~/.sshex/`。对话里：

- 每次写入后显示 `✔ 记忆 +1（共 N 条）→ ~/.sshex/MEMORY.md`
- 启动时横幅显示 `记忆 N 条 (~/.sshex/MEMORY.md)`
- `/memory` 看最近几条
- 直接问 AI“这台机器之前做过什么”，它会用 `recall` 自己去翻

AI 每次完成有意义的运维工作，会在回复末尾输出记忆块：

```
<memory>
summary: web-01 磁盘 / 使用率 91%，主要占用在 /var/log
host: web-01
detail: / 分区 91%，建议清理日志
note: web-01 日志目录增长快
</memory>
```

程序自动剥离这段并写入文件（对话界面只显示结论），同时：
- `summary` 写入 MEMORY.md 和该机器的 `last_summary`；
- `note` 追加到该机器的备注（去重，最多保留 20 条）。

MEMORY.md 开头有一段「长期约定」，可以手写所有操作都要遵守的规则（例如"生产变更前必须备份"），
AI 每次对话都会读到它。

---

## 安全设计

1. **灾难命令硬拦截**：递归删除 `/`、`/etc`、`/var`、`/usr`、`/home` 等关键目录，
   以及 `/var/lib/mysql` 等数据目录，**无论配置怎么写都拒绝执行**，`allow_patterns` 也不能放行。
   判定用代码实现而非正则，避免误伤 `rm -rf /var/log/nginx/*.log` 这类正常操作。
2. **风险命令直接执行**：`rm`、`systemctl restart`、`docker rm` 等不再逐条确认，
   AI 想做什么就做什么——按使用者的要求。
3. **密码不经过 AI**：录入服务器时密码由终端直接读取（不回显），AI 只能看到"已录入"这个结果；
   系统提示里也不包含任何密码。
4. **审计日志**：`policy.audit_log` 记录每条执行过的命令、退出码、耗时。
5. **主机指纹**：首次连接自动记录到 `data/known_hosts`，之后主机密钥变化会中止连接。
6. **高风险操作提示**：`policy.privileged_users` 中列出的用户（如 root）执行命令时会额外提示。

**注意**：服务器密码文件按你的要求未加密。如果这台机器是多人共用的，请自行用磁盘加密或
把 `data/` 目录设成只有你能访问（程序默认建目录权限为 0700）。

---

## 批量模式

原有的批量执行能力保留，适合定时巡检（不需要 AI）：

```json
"servers": [
  {"id": "web-01", "host": "10.0.1.11", "username": "ops", "key": "prod_key",
   "jump": {"server": "bastion"}, "host_key": {"policy": "accept-new"}}
],
"tasks": [
  {"name": "disk_usage", "command": "df -P -B1", "inspect": {"kind": "df"}, "tags": ["health"]}
]
```

```bash
./sshex run --dry-run            # 看会执行什么
./sshex run -g web --tags health # 按分组/标签执行
./sshex run --report ./reports   # 生成 Markdown 巡检报告
./sshex exec -H web-01 --cmd 'df -h'
./sshex check -H web-01          # 只体检这一台
```

批量模式的密钥仍可用 `keys` 段（私钥/密码/ssh-agent），支持跳板机、`proxy_command`、
上传下载（无 sftp-server 时自动退化）、超时控制、并发执行。

---

## 常见问题

**Q: 启动就报 401 / Invalid API key？**
`api_key` 写错了，或引用的环境变量没导出。用 `./sshex check` 可以直接验证模型接口。

**Q: 报 404 或 "接口路径不对"？**
`base_url` 少了 `/v1`，或 `model` 名字不对，对照上面的表格改。

**Q: AI 说要确认，但我在脚本里跑？**
脚本/管道里没有终端，风险命令会被直接拒绝——这是有意的。交互使用，或把明确安全的命令
写进 `policy.allow_patterns`。

**Q: 想换用本地模型（免费、数据不出内网）？**
装 Ollama 后 `base_url` 填 `http://127.0.0.1:11434/v1`，`model` 填 `qwen2.5`，`api_key` 留空。

**Q: 记忆写乱了怎么办？**
直接编辑 `data/MEMORY.md`，AI 下次会读到你的修改；想让它"忘掉"某段就删掉对应小节。

**Q: 密码想加密保存？**
当前按需求未加密。如需加密，可以后续加一个主密码派生 AES-GCM 的方案（要的话告诉我）。
## 安装与构建

需要 Go 1.26+（依赖 `golang.org/x/crypto`、`github.com/pkg/sftp`、`golang.org/x/term`）。

```bash
# 普通构建
go build -o sshex .

# 带版本信息
go build -ldflags "-X main.version=1.0.0 -X main.commit=$(git rev-parse --short HEAD) -X main.buildTime=$(date -Iseconds)" -o sshex .

go test ./...     # 单元测试
```

国内网络可设置模块代理：

```bash
export GOPROXY=https://goproxy.cn,direct
go mod download
```

---

## 配置文件详解

配置文件顺序刻意按 **密钥 → 身份 → 工作内容 → 策略** 组织。字段拼错会直接报错（不会静默忽略）。

### 1. `keys` —— 密钥

| 字段 | 说明 |
|---|---|
| `name` | **必填**，唯一。服务器用 `key` 字段引用它 |
| `type` | `private_key` / `password` / `agent` / `none`。可省略，程序自动识别 |
| `username` | 使用该密钥登录的用户名（服务器未写 `username` 时生效） |
| `path` | 私钥路径，支持 `~/` 与相对配置文件目录 |
| `value` | 密码明文、内嵌私钥 PEM、或 agent socket 路径。支持 `${ENV}` 引用环境变量 |
| `passphrase` | 加密私钥的口令（也可交互输入） |
| `known_hosts` | 可选，`.pub` 公钥文件；其中的公钥会自动成为引用该密钥的主机的指纹 |
| `description` | 人读说明 |

自动识别规则：

1. 有 `path` → 私钥
2. `value` 含 `PRIVATE KEY-----` → 私钥
3. `value` 形如路径且包含 `agent` → ssh-agent（留空则用 `$SSH_AUTH_SOCK`）
4. 其它非空 `value` → 密码
5. 都没有 → 免认证

示例：

```json
"keys": [
  { "name": "prod_key", "type": "private_key", "username": "ops", "path": "~/.ssh/id_ed25519" },
  { "name": "db_pass",  "type": "password",    "username": "dba", "value": "${PROD_DB_PASSWORD}" },
  { "name": "agent_key","type": "agent",       "value": "${SSH_AUTH_SOCK}" }
]
```

### 2. `identities` —— 身份介绍

描述"这台机器是谁、谁负责、干什么的"。这些内容会随每次操作展示，并写进报告。

| 字段 | 说明 |
|---|---|
| `name` | 与 `servers[].id` 对应 |
| `role` | 角色：`web` / `db` / `cache` / `bastion` …（任务可用 `role` 精确匹配） |
| `owner` | 负责人 |
| `tags` | 标签，供 `hosts` 展示与筛选参考 |
| `description` | 这台机器是干什么的 |
| `notes` | 补充说明 / 变更注意事项 |

`identities.default` 是兜底身份，服务器没有单独介绍时使用。
合并优先级：**服务器字段 > identities 里的介绍 > default**。

```json
"identities": {
  "default": { "role": "server", "owner": "运维组" },
  "servers": [
    { "name": "web-01", "role": "web", "owner": "前端组 · 张三",
      "description": "Nginx 前置机", "notes": ["nginx -t 通过后才能 reload"] }
  ]
}
```

### 3. `servers` —— 服务器

| 字段 | 说明 |
|---|---|
| `id` | **必填**，唯一标识 |
| `name` | 显示名，默认同 `id` |
| `host` / `port` | **必填** / 默认 22，支持 IPv6 |
| `username` | 登录用户；留空取 `key.username`，再留空取当前系统用户 |
| `key` | **必填**，引用 `keys[].name` |
| `group` | 分组，`run -g` / `hosts -g` 可筛选 |
| `role` / `owner` / `desc` / `tags` / `notes` | 身份信息（覆盖 identities） |
| `host_key` | 主机指纹策略，见下 |
| `jump` | 跳板机：`{"server": "bastion"}` 引用已定义服务器，或 `{"host","port","username","key"}` 直连 |
| `proxy_command` | 自定义 ProxyCommand 命令串（与 `jump` 二选一） |
| `proxy_tokens` | 同上但以数组直接 exec，不经 shell，例如 `["ssh","-W","%h:%p","bastion"]` |
| `timeout_sec` | 连接超时，默认 15 |
| `enabled` | 默认 `true`，`false` 则跳过 |

`host_key`：

| 字段 | 说明 |
|---|---|
| `policy` | `strict`（默认，必须匹配）、`accept-new`（首次记录到 known_hosts）、`insecure`（不校验） |
| `fingerprints` | 可信公钥列表，支持三种写法：`SHA256:xxx`、`ssh-ed25519 AAAA...` 公钥行、**`.pub` 文件路径** |
| `known_hosts` | known_hosts 文件，默认 `~/.ssh/known_hosts` |
| `verify` | 默认 `true`；设 `false` 只显示指纹不校验（仅限隔离网络） |

获取主机指纹：

```bash
ssh-keyscan -t ed25519 10.0.1.11 | ssh-keygen -lf -
```

### 4. `tasks` —— 工作内容

| 字段 | 说明 |
|---|---|
| `name` | **必填**，唯一 |
| `type` | `command`（默认）/ `upload` / `download` |
| `description` | 这项工作的目的 |
| `command` | 命令，支持模板变量与多行 |
| `expect` | 期望输出包含的文本 |
| `exit_code` | 期望退出码，默认 0 |
| `inspect` | 输出解析：`{"kind":"df\|meminfo\|load","mounts":[...]}`，结果与 `alert` 阈值比较 |
| `output_file` | 输出落盘，支持 `{{.Host}}` / `{{.Task}}` |
| `local` / `remote` / `mode` / `overwrite` / `exec_after` | 上传下载相关 |
| `hosts` / `groups` / `role` / `tags` | 适用范围与筛选 |
| `vars` | 模板变量（优先级高于内置变量） |
| `timeout_sec` | 默认 60 |
| `enabled` | 默认 `true` |

模板内置变量：`{{.Host}}` `{{.HostID}}` `{{.Name}}` `{{.User}}` `{{.Port}}`，
未定义的变量渲染为空串（不会破坏命令里的正则或 `{}`）。

`inspect` 用法：

- `df`：解析 `df -P`（Linux `-B1`、macOS `-k`、BusyBox 1K 均可），比较 `alert.disk_used_percent`
- `meminfo`：解析 `/proc/meminfo`，比较 `alert.mem_used_percent` / `alert.swap_used_percent`
- `load`：解析 `cat /proc/loadavg; nproc`，比较 `alert.load_per_core`

超阈值时任务状态从 `OK` 变为 `WARN`，并在输出和报告里给出告警条目。

```json
{
  "name": "disk_usage",
  "description": "磁盘使用率巡检",
  "command": "df -P -B1",
  "inspect": { "kind": "df", "mounts": ["/", "/data"] },
  "tags": ["health"]
}
```

### 5. `policy` —— 运行策略

| 字段 | 默认 | 说明 |
|---|---|---|
| `max_parallel` | 4 | 并发主机数 |
| `confirm_risk` | true | 风险命令执行前交互确认（`-y` 自动确认；非交互环境会拒绝） |
| `continue_on_error` | true | 单任务失败不阻断同机其它任务 |
| `fail_fast` | false | 某机一旦失败，跳过其剩余任务 |
| `allow_patterns` | - | 正则，命中直接放行（优先级最高） |
| `deny_patterns` | - | 正则，命中直接拒绝（在**内置黑名单之外**追加） |
| `privileged_users` | - | 高危用户列表（用于提示） |
| `audit_log` | - | 审计日志（JSON Lines），记录每条命令、退出码、耗时 |

### 6. `alert` / `report`

```json
"alert":  { "disk_used_percent": 85, "mem_used_percent": 90, "swap_used_percent": 50, "load_per_core": 2 },
"report": { "dir": "reports", "include_hosts": true }
```

---

## 命令参考

```text
sshex <命令> [选项]
```

| 命令 | 作用 |
|---|---|
| `check` | 体检：JSON 语法、结构校验、密钥可解析、逐台登录（`--no-login` 只校验配置，`-g`/`-H` 可局部检查） |
| `inv` | 打印密钥 + 服务器身份介绍 + 工作内容摘要 |
| `keys` | 密钥信息（路径、公钥指纹、被谁引用；**不打印私钥/明文密码**） |
| `tasks` | 工作内容清单 |
| `hosts` | 服务器清单及本次筛选结果（`-g` / `-H`） |
| `run` | 执行任务 |
| `exec` | 在单台主机执行临时命令（不写进配置） |
| `report` | 执行并生成 Markdown 报告 |
| `version` / `help` | 版本 / 帮助 |

公共选项：`-c/--config`、`--dry-run`、`-y/--yes`、`-v/--verbose`（可放在子命令前后）。

`run` 选项：`-g/--group`、`-H/--host`、`--tags`、`--task`、`--report <dir>`、`--timeout`、`--json`。

```bash
sshex run                                  # 全部主机、全部任务
sshex run -g web                           # 只跑 web 分组
sshex run -g web -H web-01                 # 再按主机收窄
sshex run --tags health                    # 只跑带 health 标签的任务
sshex run --task disk_usage -v             # 只跑一个任务并打印输出
sshex run --json > result.json             # 机器可读结果
sshex run --report ./reports               # 生成 Markdown 报告
sshex exec -H db-01 --cmd 'df -h'          # 临时命令
sshex exec -H web-01 'systemctl status nginx' -y
```

---

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 全部成功（可有 WARN） |
| 1 | 有主机或任务失败 |
| 2 | 配置 / 参数错误 |
| 3 | 用户取消 |
| 4 | 被安全策略拦截 |

适合放进 CI / cron：`sshex run --report ./reports || alert.sh`

---

## 安全设计

1. **危险命令黑名单永远生效**：`rm -rf /`、`reboot`/`shutdown`、`mkfs`、`dd of=/dev/sd*`、fork 炸弹等，
   即使 `confirm_risk=false` 或加了 `-y` 也会被拒绝；确需执行请在 `allow_patterns` 显式放行。
2. **风险命令二次确认**：`rm`、`systemctl stop/restart`、`kill`、`chmod/chown`、`iptables`、
   `docker rm/stop/prune`、`kubectl delete`、`DROP/TRUNCATE` 等，默认要求输入 `y`。
   非交互环境（CI）会拒绝，必须显式 `-y`。
3. **主机指纹**：默认 `strict`；`accept-new` 首次记录后严格校验；密钥变更会中止并给出修复命令。
   绝不静默接受变更的密钥。
4. **秘密不落日志**：`keys` 输出只显示打码值；密码可用 `${ENV}` 引用环境变量。
5. **审计日志**：配置 `policy.audit_log` 后，每条命令（含上传下载）、退出码、耗时以 JSON Lines 追加记录。
6. **私钥权限检查**：私钥文件权限过宽（如 0644）时加载即报错，提示 `chmod 600`。

---

## 目录结构

```
main.go       入口、子命令分发、用法说明
config.go     配置结构体、解析、默认值、校验、身份合并
cli.go        各子命令（run/exec/check/inv/keys/tasks/hosts）
runner.go     并发编排、主机筛选、任务矩阵、输出与汇总
tools.go      任务执行（命令/上传/下载）、模板渲染、结果落盘
inspect.go    巡检输出解析（df / meminfo / load）与阈值判断
policy.go     安全策略（黑名单/确认/放行）、审计日志
sshclient.go  认证方法、拨号（直连/跳板/ProxyCommand）、会话、保活、超时
sshkey.go     主机指纹解析、known_hosts 读取（含通配符/哈希主机名）、记录
sftp.go       SFTP 上传下载（远端无 sftp 时自动退化为 base64 管道）
report.go     Markdown 报告
terminal.go   终端交互（密码不回显、确认输入）
config.example.json  示例配置（合法 JSON）
files/        示例上传文件
```

实现要点：

- SSH 协议使用官方 `golang.org/x/crypto/ssh`；
- 命令经 `bash -l -c` 执行并以 base64 传递，避免引号/换行/中文被二次解析，且能拿到登录环境的 PATH；
- 远端未启用 SFTP 时，上传下载自动退化为 base64 管道，无需额外安装；
- 同一跳板机连接在批量执行中复用，避免反复登录；
- 每 30s 发送 keepalive 探测断线。

---

## 常见问题

**Q: 提示 `host_key.policy=strict` 未知主机密钥？**
先核对指纹（`ssh-keyscan -t ed25519 HOST | ssh-keygen -lf -`）再填入 `fingerprints`，
或临时改 `accept-new`，确认后把 `~/.ssh/known_hosts` 对应行固定下来。

**Q: 私钥是 `.ppk` / PuTTY 格式？**
先转换：`puttygen key.ppk -O private-openssh -o id_ed25519`。

**Q: 加密私钥怎么给口令？**
配置 `passphrase`，或留空由程序交互提示输入（不会写到磁盘）。

**Q: 没有 sftp-server 能传文件吗？**
可以，程序自动退化为 base64 管道（依赖远端 base64 命令，通常都有）。

**Q: 任务超时后远端进程还在？**
程序会发送 SIGTERM 并关闭会话；若远端命令自行 daemon 化（如 `nohup`），需自行处理。

**Q: 想只用单台机器快速验证？**
`sshex check -H web-01` 只登录这一台；也可以用
`sshex exec -H web-01 --cmd 'echo ok'` 快速验证登录链路。

**Q: run 会执行所有任务吗？**
默认执行全部「已启用且适用于该主机」的任务；用 `--task` / `--tags` / `-g` / `-H` 收窄，
`--dry-run` 可先确认执行矩阵。
