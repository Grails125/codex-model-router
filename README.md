# Codex Model Router

让同一个 Codex 桌面客户端的原生模型菜单同时显示 OpenAI 和 DeepSeek。选择模型后，本地路由会把 Responses 请求转发到对应渠道，支持流式输出和工具调用。

这是独立社区项目，与 OpenAI、DeepSeek 官方无隶属关系。当前版本为 **1.0.3**。

## 下载安装

先安装 Codex 桌面客户端并登录 ChatGPT，同时准备可用的 DeepSeek API Key。

| 平台 | 下载 | 解压后的安装入口 |
| --- | --- | --- |
| Windows x64 / Arm64 | [Windows 安装包](https://github.com/Grails125/codex-model-router/releases/download/v1.0.3/CodexModelRouter-v1.0.3.zip) | 双击 `install-windows.cmd` |
| macOS Apple Silicon / Intel | [macOS 安装包](https://github.com/Grails125/codex-model-router/releases/download/v1.0.3/CodexModelRouter-v1.0.3-macOS.tar.gz) | 双击 `Install-Codex-Model-Router.command` |

[全部发布文件和 SHA-256 校验文件](https://github.com/Grails125/codex-model-router/releases/tag/v1.0.3)

必须先完整解压，再运行安装入口。安装包自带对应架构的程序，使用时不需要 Python、Node、WSL 或 Homebrew。

第一次安装会要求输入 DeepSeek API Key，输入内容不会回显。如果已有兼容的 DeepSeek 配置，可以直接回车尝试复用。安装完成后：

1. 结束正在执行的 Codex 任务。
2. 完全退出并重新打开 Codex。
3. **新建聊天**，再从输入框下方的原生模型菜单选择模型。

DeepSeek 模型为 `DeepSeek-Flash` 和 `DeepSeek-V4-Pro`。OpenAI 模型来自当前登录账户可访问的认证模型目录。安装器保留原有 OpenAI 登录信息。

上面的 v1.0.3 安装包使用自定义提供商 `OpenAI + DeepSeek`，在该提供商下新建聊天后即可切换模型。从当前源码构建的安装器使用内置 `openai` 提供商连接本地路由，原有 OpenAI 聊天可以继续使用。模型菜单把两个 DeepSeek 模型排在 OpenAI 模型之后。由旧版自定义路由提供商创建的聊天仍保存原提供商标识。

### 命令安装

在解压后的 `CodexModelRouter` 目录中运行。Windows：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\install-windows.ps1
```

macOS：

```sh
chmod +x install-macos.sh
./install-macos.sh
```

macOS 程序尚未签名或公证，系统可能阻止首次打开。核对来源和校验值后，可按 macOS 系统设置或 Finder 提供的手动允许方式打开。

### 可选环境变量与自定义目录

交互输入密钥可以避免将密钥写入命令历史。自动化安装也可通过安装进程的 `DEEPSEEK_API_KEY` 环境变量提供密钥，安装后清除该变量。不要把真实密钥写进脚本或提交到仓库。

Windows 自定义 Codex 目录和首选端口：

```powershell
.\install-windows.ps1 -CodexHome 'C:\path\to\.codex' -Port 49627
```

macOS 自定义 Codex 目录和首选端口：

```sh
CODEX_HOME_FLAG="$HOME/custom-codex" CODEX_ROUTER_PORT=49627 ./install-macos.sh
```

首选端口被占用时，安装器会自动选择空闲的本地端口。

### 修改 DeepSeek API Key

在完整解压的安装包中运行以下入口，输入新 Key 并回车：

| 平台 | 双击入口 | 命令入口 |
| --- | --- | --- |
| Windows | `change-deepseek-key-windows.cmd` | `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\change-deepseek-key-windows.ps1` |
| macOS | `Change-DeepSeek-API-Key.command` | `./change-deepseek-key-macos.sh` |

输入内容隐藏，直接回车取消并保留原 Key。脚本仅原子替换本机 `deepseek.key`，下一次 DeepSeek 请求自动使用新 Key，**无需重启路由或 Codex**；正在执行的请求继续使用原 Key。更新不修改 OpenAI 登录、模型菜单或其他配置，也不会将 Key 写进命令行参数或输出。

自定义 Codex 目录时，Windows 在脚本后添加 `-CodexHome 'C:\path\to\.codex'`，macOS 使用 `CODEX_HOME_FLAG="$HOME/custom-codex" ./change-deepseek-key-macos.sh`。

自动化可将 Key 通过标准输入提供给 `codex-router set-key --stdin`，或仅为该进程设置 `DEEPSEEK_API_KEY` 后执行 `codex-router set-key`；支持 `--codex-home PATH` / `--data-dir PATH`。程序不接受直接传入 Key 的命令行参数。保存成功表示本机文件已更新，未调用 DeepSeek 验证有效性；Key 是否可用由后续 DeepSeek 请求确认。

## 工作方式与配置范围

```text
Codex 原生模型菜单
        |
        v
127.0.0.1 本地 Responses 路由
        |
        +-- OpenAI 模型 ----> https://chatgpt.com/backend-api/codex
        |
        +-- DeepSeek 模型 --> https://api.deepseek.com
```

路由只监听回环地址，默认首选端口为 `49627`，请求需要随机生成的本地凭据，并拒绝浏览器来源请求。它合并认证后的 OpenAI 模型目录与内置 DeepSeek 定义；ChatGPT 认证信息只发往 OpenAI，DeepSeek 请求只携带 DeepSeek 密钥。

将对话切回 OpenAI 时，路由会去掉其他模型的明文 reasoning 内容，保留可见消息及工具调用与结果。这项适配不代表不同模型的所有能力完全一致。

安装器会备份 `~/.codex/config.toml`，并保留 MCP、插件、hooks 和信任设置等配置。当前源码的安装器设置 `model_provider = "openai"`，通过带本地凭据的 `openai_base_url` 连接路由，并禁用路由尚未支持的请求压缩。卸载时恢复原设置。运行文件位于 `~/.codex/codex-model-router/`。

Windows 使用当前用户的计划任务 `Codex Model Router`，macOS 使用当前用户的 LaunchAgent `io.openai.codex-model-router`，在登录后启动后台路由。

从 1.0.3 起，Windows 安装目录中的 `codex-router.exe` 是无窗口程序，后台运行不显示命令行窗口。安装器从本项目的程序生成该版本，复用匹配的旧任务，无需修改原任务启动路径；同目录另存 `codex-router-cli.exe`，用于安装、诊断、修改密钥和前台调试。安装包内的程序仍为命令行版，供安装脚本使用。macOS 继续通过 LaunchAgent 静默运行。安装、诊断和输入 Key 时打开的终端可以在操作完成后关闭；后台启动输出写入 `service.log`。

DeepSeek 密钥保存在本机 `deepseek.key` 中，不会打印到安装输出。路由审计日志记录时间、模型、渠道、HTTP 状态、流式标志和连接诊断字段，不记录原始异常文本、请求头、密钥或对话内容。Codex 客户端的诊断日志可能包含带本地凭据的路由 URL。分享日志前先脱敏，个人 `config.toml`、`auth.json`、`settings.json`、`deepseek.key` 和安装收据也应妥善保管。

## 诊断与卸载

在原安装包解压目录中运行：

| 操作 | Windows | macOS |
| --- | --- | --- |
| 检查服务和配置 | `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\doctor-windows.ps1` | `./doctor-macos.sh` |
| 停止路由并恢复原提供商配置 | `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\uninstall-windows.ps1` | `./uninstall-macos.sh` |

卸载会保留后来修改的无关配置，同时保留本机备份和程序以便恢复。带时间戳的配置备份位于 `~/.codex/codex-model-router/backups/`。

模型菜单没有更新时，先运行诊断命令，再完全重启 Codex 并新建聊天。若请求出现 502，需继续检查本机网络、代理和上游连接；本地服务健康通过并不代表每次上游请求都会成功。

1.0.2 会复用上游 TCP/TLS 连接，并仅在确认 Responses 请求尚未发送的暂时建连故障后重试一次。请求可能已发送、收到上游 HTTP 错误、用户取消或响应流中断时，不自动重放请求。流中断会中止下游连接，避免正常 EOF 掩盖失败。

`requests.jsonl` 的 `stage` 可区分 `dns`、`connect`、`proxy_connect`、`tls`、`send`、`response_headers` 和 `response_body`；`error_class` 提供固定错误分类。`retry_stage` 和 `retry_error_class` 表示发生过安全重试。`proxy_used=false` 表示未使用显式 HTTP 代理，系统 TUN 仍可能处理该连接。此版本不会自动更改系统代理或切换上游渠道。

## 从旧版升级

重新运行新版安装入口即可。1.0.1 修复了旧 Python 路由占用端口时，新服务未启动却被误报为成功、从而导致 DeepSeek 模型消失的问题：

- 检测到端口被占用时自动避让，不终止旧 Python 路由，以便原有聊天完成任务。
- 健康检查验证本地凭据、服务版本和随机 challenge 的 HMAC，不能仅凭 HTTP 200 判断成功。
- 重装保留本地路由凭据；Windows 复用匹配的计划任务，替换程序前停止该任务。
- 安装失败时恢复之前的配置、运行设置、凭据、收据及程序。

升级完成后仍需重启 Codex 并新建聊天。

## 验证范围

| 平台 / 检查 | 当前状态 |
| --- | --- |
| Windows x64 | 已实际验证安装、现有任务升级、健康检查、客户端模型列表和真实模型请求 |
| Windows Arm64 | 已构建，未在 Arm64 设备运行验证 |
| macOS Apple Silicon / Intel | 已构建并检查包内执行权限、脚本和 LaunchAgent XML；未在真实 Mac 上验证安装或运行 |
| Go 测试与静态检查 | 包含无窗口程序、任务迁移、密钥修改和运行中生效回归；`go test ./...` 和 `go vet` 通过 |

双平台构建成功不等于双平台实机验收。macOS 安装包以未签名、未公证的形式提供。

## 从源码构建

需要 Go 1.23 或更新版本。测试和静态检查：

```sh
go test ./...
go vet ./...
```

仅构建当前平台的程序：

```sh
go build -trimpath -o codex-router .
```

Windows 上构建全部四种架构及发布安装包：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\build.ps1 -Version 1.0.3
```

完整打包还需要 Python，仅用于生成保留 macOS 执行权限的 tar.gz，不是安装后的运行依赖。输出位于 `dist/`，包括 ZIP、macOS tar.gz 和各自的 SHA-256 校验文件。`deepseek_models.json` 保存 DeepSeek 模型定义，`models.go` 将其嵌入程序；远程 OpenAI 模型目录由路由按客户端版本和账户身份获取。
