# Codex 补丁维护

此分支是个人使用的 Magpie fork，不是官方发布。源码基线为上游
`v0.1.4288`（`c6a67dd412b372ca1757ecb96040fcc6e4b2a664`）。

## 保留的改动

| 改动 | 来源 | 行为及边界 |
| --- | --- | --- |
| 中断后按响应 ID 继续 | 原本机提交 `475cc918987c28fd10e199b68e2740793bc47251`；[上游 #6443](https://github.com/yetone/magpie/issues/6443) | 仅对明确接受的流式中断，先保存完整输入，再确认中断；继续时不重放已丢弃的部分输出。未知 ID、普通断开和上游失败不因此缓存。 |
| 上游关闭状态诊断 | 原本机提交 `5e33655c14aba54514de5e608ec16f656aea1315` | 在异常被转成 EOF 前记录关闭码、帧计数、时间和取消标记；不记录请求正文、鉴权头或关闭原因原文。诊断不是断流修复。 |
| Windows 完整构建 | `build/windows/build-codex-patches.ps1` | 沿用官方图标、DPI manifest、版本资源、UI 打包和控制台 launcher。避免只执行 GUI 编译而漏掉 exe 图标资源。 |

2026-11-16 查询时 #6443 仍为 open，暂无回复；上游此版本仍在取消后
跳过 `rs.keep`。issue 状态仅是线索，是否修复以对应版本的回归结果为准。
另一个报告 #6301 的“完成与中断同时到达”竞态不在此补丁保证范围。

## 更新与自动更新

同一会话的大请求（完整 WebSocket 消息至少 1 MiB）在新连接上发送成功后，
若首个应用数据帧之前收到 1011，仍按原有流程回退 HTTP。此后五分钟内，
同一后端、账户、会话和模型的同类请求优先复用已有 WebSocket，没有可用
连接才直接走 HTTP。冷却不因命中延长，只保存在进程内，最多记录 128 项。
显式携带 previous_response_id 的请求不使用该捷径。慢但健康的连接沿用原有
等待行为。阈值与五分钟期限是本 fork 的传输策略，线上收益须以首字耗时验证。

在 `lzc/codex-patches` 分支上，把选定的官方发布合入并解决冲突；保留
每项补丁的来源和测试。先在未打补丁的新版本运行中断续接回归，若仍失败
再移植生产改动。若官方已修复，以同一测试证明后移除重复实现。

补丁版标识为 `dev-lzc-<上游版本>`。这是有意保留的 source build：官方
自动替换会覆盖尚未合并的补丁，不能只伪装成官方版本来打开自动更新。
当前采用人工审核后更新 fork；没有后台自动拉取、自动合并或自动发版。
待所需修复全部进入官方且不再需要本机诊断时，可安装校验过的官方包，恢复
官方自动更新。不改变或绕过官方更新签名检查。

## 构建与安装

需要对应 `go.mod` 的 Go 工具链。Windows x64：

```powershell
& ./build/windows/build-codex-patches.ps1 -OutputDirectory C:\path\outside-repo\package -Go C:\path\go.exe -BaseVersion 0.1.4288
```

脚本生成 `magpie.exe` 和包含源码 revision、SHA-256、版本的 `build.json`。
输出必须在工作树外；图标生成器固定为官方使用的 `go-winres@v0.3.3`。

结束工作、正常退出 Codex 和 Magpie（包括托盘）后安装：

```powershell
& ./build/windows/install-codex-patches.ps1 -PackageDirectory C:\path\outside-repo\package -Target C:\path\magpie.exe -ExpectedCurrentSha256 <安装前核实的SHA256>
```

安装器检查旧文件和新包摘要、图标资源，检测活动进程并拒绝打断会话，备份
原 exe，再替换文件。只刷新指向同一目标的开始菜单快捷方式图标；不修改
账户、模型、路由、代理和用量。备份位置写入本机 `installed.json`。
回退也应先退出应用，检查当前文件和备份摘要，再恢复该备份。

重开后通过 `magpie status --json` 确认真正运行的版本，再检查任务栏图标和
正常聊天/工具调用。磁盘文件已更新不等于运行中的进程已更新。

## 验证

相关用例：

```text
go test -tags nogui ./internal/gateway -run '^TestResponsesSocketInterruptedResponseContinues$' -count=1
go test -tags nogui ./internal/gateway -run 'TestResponsesSocket|TestCodexSocket|TestCodexKept|TestCodexTurn' -count=1
```

中断续接回归覆盖新会话、有已完成历史、输入不丢失/不重复、丢弃部分输出、
继续后的正常增量续接。关闭码测试覆盖正常关闭、策略关闭和无关闭帧。
测试使用项目已有隔离入口及本地模拟上游，不调用真实模型或账户。
完整检查要求见 `docs/code-standards.md`；每次交付应如实记录已跑和未跑项，
构建成功不等于真实桌面验收。机器日志、凭据、配置、安装备份和产物不入 Git。
