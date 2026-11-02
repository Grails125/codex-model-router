# v1.0.2

改善 Responses 转发的连接恢复，并让 502 错误具备可定位的安全诊断。

- 将上游每个主机的空闲连接池扩大到 16，减少重复 TCP/TLS 建连。
- 仅在确认请求尚未发送的暂时 DNS、TCP、TLS 或代理 CONNECT 故障后重试一次；不自动重放已发送的 POST，不重试上游 HTTP 错误、取消或流中断。
- 禁止客户端内部重放带幂等标记的已发送 POST；关闭自动跟随重定向。
- 安全日志增加失败阶段、固定错误分类、数字错误码、耗时、尝试次数、连接复用和代理使用情况，不记录原始异常、凭据或对话内容。
- 响应体传输中断时中止下游 HTTP 流，让客户端识别失败。
- 新增 8 项传输回归；共 28 项测试和 `go vet` 通过。

下载 ZIP（Windows）或 macOS tar.gz 后完整解压，重新运行安装入口。安装器保留本地路由凭据，并备份配置。macOS 双架构已构建但未在真实 Mac 上验收，程序未签名、未公证。

# v1.0.1

在同一个 Codex 桌面客户端的原生模型菜单中使用 OpenAI 和 DeepSeek，并按所选模型路由到各自渠道。无需 Python、Node 或额外客户端运行时。

## 下载和安装

- Windows x64 / Arm64：下载 `CodexModelRouter-v1.0.1.zip`，完整解压后双击 `install-windows.cmd`。
- macOS Apple Silicon / Intel：下载 `CodexModelRouter-v1.0.1-macOS.tar.gz`，完整解压后双击 `Install-Codex-Model-Router.command`。
- 准备 DeepSeek API Key，按提示完成安装；随后完全退出并重开 Codex，再新建聊天。
- 每个安装包附有 SHA-256 校验文件。

## 本版修复

- 旧 Python 路由或其他进程占用端口时自动避让，保留正在使用旧路由的聊天。
- 健康检查验证本地凭据、服务版本和 challenge HMAC，避免把其他服务的 HTTP 200 误判成安装成功。
- Windows 复用匹配的计划任务，替换程序前停止该任务；重装保留路由凭据。
- 安装失败恢复配置、运行设置、凭据、安装收据及程序。
- 支持没有 config.toml 或省略默认 model_provider 的首次安装。
- 修正 macOS LaunchAgent 升级时的 bootout 服务目标。

## 验证与限制

- 20 项 Go 测试和 go vet 通过，已验证模拟文件锁失败后的安装回滚。
- Windows x64 已实际验证安装、已有任务升级、服务身份检查、原生模型列表及真实 DeepSeek 流式请求。
- Windows Arm64 和 macOS 双架构已构建；Windows Arm64 未做设备运行验收，macOS 未在真实 Mac 上验证安装或 launchd 运行。
- macOS 程序未签名、未公证；请核对来源与校验值，并按系统提示手动允许首次运行。

本项目为社区项目，与 OpenAI、DeepSeek 官方无隶属关系。完整使用说明见仓库 README。
