# Codex 连接对应诊断

本分支基于实际使用的 `475cc918` 构建，保留原有中断续接修复；不同时升级
上游版本、改变路由、增加重试或延长超时。它是诊断版，不代表已经修复断流。

## 能对应的边界

`codex-transport-v2` 事件记录路由编号、请求编号、会话摘要、物理连接编号、
本地和对端地址、WebSocket 轮次编号。物理连接复用时，每个请求重新绑定；
首次拨号所属请求不被当成连接的永久拥有者。原生 Codex 后端与经账户路由
的 Codex 请求均覆盖。

TLS 下方的连接包装只委托原来的读、写、关闭和 deadline 操作，记录首次
原始读写错误及其发生时间、字节计数、主动关闭开始和结束时间。WebSocket
读取失败先记录，再沿原路径关闭。这样可区分原始字节流先结束、协议层
先结束、请求 context 先结束，以及错误之后的清理。`request_context_done`
也可能是成功后的正常清理，不能单独解释为用户取消。

已有 Ping/Pong 的接收时间与次数通过库提供的回调记录；Ping 回调仍返回
true，保留自动 Pong。诊断不主动发 Ping，不增加读写期限，不按正文静默
关闭连接。原始连接每 30 秒记录一次计数快照，不复制任何字节内容。

纳秒时间和进程启动标记在 JSON 中以十进制字符串输出，避免 JavaScript
数字精度损失。错误只输出固定类别和数字系统错误码；不输出正文、鉴权头、
会话原始标识、错误原文或远端 Close reason。异步日志队列有界，丢弃数可见。

## 与 Clash 对应

`tools/codex-connection-diagnostics/collector.mjs` 从真实桌面进程上下文读取
Magpie 日志，读取已有 Mihomo 命名管道 API，并记录源地址、代理监听端口和
连接创建时间均一致的对应关系。仅源端口相同不足以建立对应，避免端口复用
误判。保存 Clash 连接编号的摘要、节点链和累计流量，以及 Codex 重试时间。
日志保留 14 天，上限 128 MB，不改代理、节点、TUN、账户或模型。

本地 `collector-config.json` 填写 node、powerShell、wsModule、codexHome 的
绝对路径。wsModule 指向已安装 `ws` 的 ESM wrapper；配置与采集证据不提交。
`Start.ps1` 使用已有 Explorer 桌面上下文启动，避免打包应用的文件虚拟化。
写入采集目录的 `STOP` 文件可正常停止该独立采集进程。

对应仍有边界：HTTP/2 可让多个请求共享连接；纳秒日志不是抓包；原始 TCP
EOF 不能独自区分远端代理入口与 OpenAI。只有对应到同一连接的节点错误、
协议终止、控制帧和 TCP 事件才能支持进一步归因。没有控制帧不是失联证明。

## 构建、安装、验证

在本分支使用 `build/windows/build-connection-diagnostics.ps1` 构建，它要求
源码已提交，保留官方图标、manifest、UI 资源和控制台 launcher，输出位于
仓库外。`build/windows/install-connection-diagnostics.ps1` 核验新旧摘要、
备份当前 exe，并拒绝在 Codex 或 Magpie 仍运行时替换；额度悬浮球独立查询
进程不算活动聊天。它不终止任何宿主，不修改账户和路由。

本地协议测试覆盖 kept WebSocket 的两轮绑定、TLS/CONNECT 对应、原始 EOF
在清理之前记录且回复仍失败、自动 Pong 保留，以及日志脱敏。分支 CI 做
Linux race 检查。运行中的程序版本、采集日志对应成功、真实断流定位是独立
验收步骤，不能以编译或模拟测试代替。
