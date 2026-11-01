# TermHive 功能发展报告与路线图

> 基于 v0.4.4 阶段现状整理（2026-10-27 首次成文，此后持续更新），对标 Xshell / Xftp / MobaXterm / FinalShell / Termius / Tabby / WindTerm / WinSCP 等市面主流远程工具，分析功能差距并规划发展路线。项目前身为工程名 DBX Shell，自 0.1.0 起定名 TermHive。

---

## 一、当前能力盘点（v0.4.4）

| 能力域 | 已实现 |
|---|---|
| 连接体系 | 接入 DBX 原生 connection-provider（连接列表统一存储/编辑/测试），密码/私钥两种认证，认证方式智能回退，键盘交互认证，用户名/密码防呆校验，keepalive，字符编码（UTF-8/GBK/GB18030），登录后执行命令 |
| 终端 | xterm.js 终端，同一连接多会话（内部标签栏），断线"重新连接"覆盖层，回放修复（上划空白已修），共享 xterm 实例切换 reset |
| 文件管理 | SFTP 浏览、上传/下载、多选批量下载（zip 打包）、目录上传、拖拽上传（宿主 onDrop）、路径导航归一、文件面板跟随终端 cwd（OSC 7 shell 集成，即时事件驱动，可开关）、大文件分段预览（2MB 窗口导航） |
| 浮窗 | DBX Shell Mini 浮动迷你终端 |
| 工程 | proto/e2e 自动化测试（32 断言）、飞行记录仪诊断（diag.log）、GitHub Actions 发布流水线（Release 自动打包 .dbxp） |

**v0.4.4 时点明确的短板（✅ 项已修复，保留原始记录便于追溯）：**

1. ✅ 🔴 **不校验服务器指纹**（`ssh.InsecureIgnoreHostKey()`）——存在中间人风险。**已于 v0.4.5 修复**（TOFU 指纹校验，见 P0 表）。
2. ✅ 🔴 **specs.json 明文落盘**（含密码）——Windows 下 0600 权限形同虚设。**已于 v0.4.5 修复**（密封信封加密，见 P0 表）。
3. ✅🟡 终端无查找、链接不可点——**已于 v0.5.0 补齐**（search / web-links addon）；字号/字体可调、复制粘贴规则选项仍待做。
4. 🟡 断线只能手动点"重新连接"——待做（自动重连）。
5. 🟡 文件面板无预览、无权限/压缩操作、传输无进度速度/续传——分段预览已于 2026-10-30 落地（见 2.2 表），其余待做。

---

## 二、重点需求方案设计（立项时点名的两项）

### 2.1 日志着色与自定义风格

**目标**：看日志时 ERROR/WARN/INFO 等不同级别一目了然；支持自定义规则与主题。

**分三层实现，彼此独立、可单独交付：**

**① 终端配色主题（基础层）**
- xterm.js 的 `theme` 对象可直接换配色。预置几套经典主题（One Dark、Dracula、Solarized、GitHub Light…），支持导入 iTerm2 / Windows Terminal 的配色 JSON（社区主题资源极其丰富，导入即用）。
- 默认跟随 DBX 宿主外观（已注入 `data-dbx-theme`，亮暗自动切换），手动选择后覆盖。
- 工作量小、收益直接，建议最先做。

**② 日志着色引擎（核心层）**
- **内置规则集**（开箱即用）：`ERROR/FATAL` 红、`WARN` 黄、`INFO` 蓝、`DEBUG` 灰；时间戳、线程名、IP、URL、数字弱色显示；Java/Python 异常堆栈首行标红。
- **实现位置在渲染层而非字节流**：xterm.js 支持 Decoration API（对匹配行叠加前景色/背景色标记），不修改原始输出。这样飞行记录仪、会话回放、复制出来的内容都不受污染。关键词高亮还可配合 search addon 的 `highlightAllMatches`。
- **用户自定义规则**：规则 = 名称 + 正则 + 颜色（前景/背景/加粗）+ 生效方式（整行/仅匹配段）。规则集可保存、启停、导出导入，可按连接绑定（例如给生产环境连接绑一套"高危操作标红"规则）。存储用 DBX storage API（单值 256KiB / 库 1MiB，存规则集绰绰有余）。
- 参考对象：lnav / grc 的规则思想、FinalShell 的日志着色、Tabby 的主题体系。

**③ 日志模式（场景层）**
- 文件面板对日志文件一键进入"日志模式"：自动执行 `tail -n 200 -f <path>`，新增内容滚动跟随。
- 滚动锁定（ScrollLock）：往上翻阅时暂停跟随，回到底部自动恢复。
- 停止跟随 = 结束远端 tail 进程。
- 这一层让 ①② 落在真实使用场景里："双击日志文件 → 着色 + 实时滚动 + 翻阅不跑屏"。

### 2.2 免下载文件预览（对标 Xftp）

**目标**：不把文件下载到本地就能看内容；进一步能编辑保存。

**关键前提**：当前后端文件 RPC 只有 `fs/list / stat / mkdir / remove / rename`，传输走 `transfer/startDownload` 整文件下载。**需要新增 `fs/read` 范围读 RPC**——SFTP 协议本身支持按 offset 读文件句柄（`ReadAt`），只拉需要的字节，不落临时文件，这正是"免下载预览"的技术基础：

- `fs/read { path, offset, length, maxBytes }` → 返回分块数据（注意 UI 桥二进制帧上限 8MiB，分块+确认即可，与现有 transfer 通道同套路）。

**预览面板能力（按优先级）：**

| 类型 | 做法 | 限制策略 |
|---|---|---|
| 文本预览 | ✅ 已实现（2026-10-30）：fs/read 窗口读取（2MB/段，offset 参数），编辑器分段浏览（开头/上一段/下一段/末尾+下载完整文件）；编码转码复用连接的 encoding 设置 | 行号、关键词高亮待做 |
| 日志预览 | 直接套用 2.1 的着色规则——**两个需求在此合流**：不下载也能看着色的日志 | 同上 |
| 图片 | png/jpg/svg/gif 转 data URL 直接显示 | 限 ≤2MB |
| 十六进制 | 二进制检测（前 4KB 出现 NUL 即判二进制）→ hex dump 视图 | 只读前 N KB |
| 压缩包浏览 | zip/tar.gz 内列表浏览（zip 靠尾部中央目录随机读，tar 顺序读头部），点条目再走 fs/read 预览 | Xftp 都没有的加分项 |
| 远端编辑 | 预览→"编辑"→保存：新增 `fs/write`，先传临时文件再 rename 原子覆盖；保存前做 mtime 冲突检测；自动留 `.bak` 备份 | 仅文本；完整文件需拉取（编辑场景可借鉴 WinSCP 的"临时目录+保存回传"模型） |

**安全护栏**：预览默认上限 2MB；无法识别的类型不硬解析，只给"下载"引导；预览只读，写操作必须显式进入编辑态。

**参考**：Xftp 的"用记事本打开"实际是下载到临时文件再打开；WinSCP 的远端编辑是"临时目录 + 保存回传 + 监听变化"。我们的纯流式预览在"只看不改"场景更干净，编辑场景借鉴 WinSCP 模型。

---

## 三、对标市面工具的功能差距分析

| 工具 | 它的核心特色 | 对本项目的借鉴点 |
|---|---|---|
| **Xshell** | 会话管理成熟、隧道管理、快速命令栏、宏、会话日志录制 | 快速命令栏（一键发常用命令）、会话日志带时间戳落盘 |
| **Xftp** | 双窗格本地↔远端同步浏览、远端文件编辑、"保持远程目录最新" | 远端编辑、双面板对比同步（P3） |
| **MobaXterm** | 多协议全家桶、内嵌 SFTP 跟随终端 cwd、宏录制 | 内嵌跟随 cwd 我们已实现；宏录制可后期考虑 |
| **FinalShell** | 服务器监控仪表盘（CPU/内存/网络/磁盘实时曲线）、进程管理器、GPU 监控 | 监控面板：定时跑 `top/free/df` 解析 + 迷你曲线；进程列表 + kill；`nvidia-smi` GPU 卡片。**这是它最拉粉的功能，建议尽早做** |
| **Termius** | 跨端同步、密钥管理（生成/导入）、代码片段库、端口转发管理 UI | 密钥生成器（ssh-keygen 图形化）、片段库（含变量）、端口转发管理界面 |
| **Tabby** | 插件生态、主题丰富、分屏、快捷键体系 | 主题系统、终端分屏、快捷键可配置 |
| **WindTerm** | 极致性能、命令面板（Ctrl+Shift+P）、会话箱 | 命令面板、性能基准（大输出吞吐、低内存） |
| **WinSCP** | 断点续传、传输校验、目录同步、"keep remote up to date"、远端编辑 | 传输队列的续传/重试/校验；同步浏览 |
| **SecureCRT** | 日志选项极细（插入时间戳、按连接分流）、键映射 | 会话日志带时间戳、按连接保存目录 |

**结论**：本项目当前 ≈ "Xshell 基础终端 + Xftp 基础文件面板"。差距最大且用户感知最强的三块：**日志/监控场景**（着色、tail、仪表盘）、**文件能力纵深**（预览、编辑、续传）、**传输体验**（进度/续传/校验）。

---

## 四、发展路线（按优先级）

### P0 — 安全与基本盘（建议 1~2 个版本内完成，上 GitHub 前至少做前两项）

| 项 | 说明 |
|---|---|
| ✅ 服务器指纹校验（v0.4.5 已完成） | 首次连接自动记录指纹（TOFU，`known_hosts.json`），指纹变更拒绝连接并给出告警与重置方法；`hostkey/list` / `hostkey/remove` RPC 供后续 UI 管理 |
| ✅ specs.json 加密（v0.4.5 已完成） | 密封信封格式：Windows DPAPI / 其他平台 AES-256-GCM + 密钥文件；启动时自动迁移旧明文文件；内存明文保留（"密码可取回"不受影响） |
| 🟡 终端基础体验 | search addon（查找+全部高亮）、web-links（链接可点）、字体/字号/回滚行数设置、复制粘贴规则选项（选中即复制/右键粘贴） |
| 🟡 断线自动重连 | 指数退避自动重拨（可关），当前只有手动按钮 |
| ✅ 仓库门面 | LICENSE 已添加（MIT）；README 截图与安装指引待补——开源后的第一印象 |

### P1 — 你点名的两项 + 日志场景（v0.5 主题）

1. ✅ 终端配色主题（v0.7.0 已完成：内置 One Dark/Dracula/Solarized Dark/Nord/GitHub Light + 跟随 DBX，即选即生效；iTerm2 导入待做）
2. ✅ 日志着色引擎（v0.7.0 已完成：内置级别规则 + 自定义正则/颜色/整行片段/启停/增删，ANSI 注入渲染层实现，全屏程序自动放行，设置持久化 settings.json）
3. 日志模式（tail -f + 滚动锁定 + 搜索）
4. `fs/read` + 文件预览（文本/图片/HEX/压缩包）
5. 会话日志落盘（带时间戳，按连接分目录）
6. 快速命令/代码片段栏（storage 保存，一键发送，支持占位变量）

### P2 — 传输与运维能力（v0.6 主题）

1. `fs/write` + 远端编辑（预览→编辑→保存，原子写+备份）
2. 传输队列增强：实时速度/ETA/暂停恢复/**断点续传**/失败重试/大小校验
3. 文件操作纵深：chmod 权限 UI、远端压缩/解压（zip/tar.gz 一键）、隐藏文件开关、排序/过滤/搜索、收藏路径
4. **监控仪表盘**（FinalShell 式：CPU/内存/磁盘/网络曲线 + 进程管理 + 可选 GPU 卡片）
5. 端口转发管理（本地/远程/动态 SOCKS，会话级）
6. rz/sz：直接支持 ZMODEM，或优先支持 **trzsz**（现代替代，兼容 tmux，实现更简单）

### P3 — 平台化与差异化（v0.7+ / 1.0）

- 多会话广播输入（内部标签栏已就绪，加"同步输入"开关即可）
- 终端分屏
- 连接分组/标签/搜索；导入 Xshell（.xml）/ PuTTY（reg）会话
- 密钥生成器（ssh-keygen 图形化）、known_hosts 管理
- 命令面板 + 自定义快捷键
- 双面板本地↔远端同步浏览（WinSCP "keep up to date"）
- mosh（弱网漫游）；Telnet/串口（按需）
- AI 辅助（报错解释/命令生成——与 DBX 数据库场景天然契合，可作为差异化卖点）
- 跨平台构建（Go 交叉编译成本低，主要是 macOS/Linux 测试矩阵）
- 自动更新检查

### 明确不做（范围控制）

- X11 转发、RDP/VNC、FTP 协议（已明确移除，文件管理走 SSH+SFTP）
- 多标签页管理（宿主 DBX 已管，插件做内部会话标签即可）
- 插件生态/脚本市场（超出单人项目体量）

---

## 五、平台约束备忘（做功能前先看）

| 约束 | 影响 |
|---|---|
| UI 桥二进制帧上限 8MiB / sidecar 64MiB | 预览、传输必须分块+确认；禁止单个大 JSON |
| storage 单值 256KiB / 库 1MiB | 规则集、片段库够用；主题 JSON 注意体积 |
| floating 每 contribution 仅一个 | 浮窗内做会话切换（现状即如此），不能开第二个浮窗 |
| fileTransfer 文件夹展开上限 2000 文件 / 8 层 | 大目录树上传有上限 |
| 宿主不感知网络层断开（绿点常亮） | 断线状态以 UI 的 shell.closed 为准，勿当宿主 bug 排查 |

---

## 六、版本号与发布约定

**正式规约见 [docs/VERSIONING.md](VERSIONING.md)（2026-10-28 起施行）**，要点：

- 语义化版本：MINOR=规划好的功能批次，PATCH=修复批次；**版本号只在发布点前进**，开发过程不动版本号；
- 中间真机验证用预发布号（`0.9.0-rc.1`），不占用正式版本（打包器已实测接受，客户端安装待一次真机确认）；
- 发版唯一入口：`npm run release:patch | release:minor`（强制要求先写 CHANGELOG 条目，一致地升 manifest 与 main.go）；
- `npm run check` 与 CI 硬校验：版本一致性 + **CHANGELOG 必须覆盖当前版本**（版本前进而无变更记录=构建失败）；
- 历史：0.1.0~0.8.1 期间"每次修改升一版"的做法不符合开发规范，已废止并补记 CHANGELOG。

DBX 平台约束回顾：按版本号目录安装 + 逐文件 sha256 校验，manifest 版本必须与 main.go 一致——这解释了"为什么装新包必须换版本号"，但**只有发版才打安装包**，日常开发在本地测试闭环。

**GitHub 上传约定**：
- 上传：`manifest.json`、`dbx-plugin.toml`、`package.json`、`package-lock.json`、`backend/`（源码 + go.mod/go.sum）、`ui/`（index.html、vite.config.ts、tsconfig.json、src、package.json、package-lock.json）、`scripts/`、`tools/`（testsrv 源码、ptyserver 捕获脚本）、`assets/`、`.github/`、`.gitignore`、`README.md`、`CHANGELOG.md`、`docs/`、`LICENSE`（MIT，已添加）。
- 不上传（现有 .gitignore 已覆盖）：`dist/`（打包产物，发布时 CI 自动打进 GitHub Release，仓库不放）、`.dbx-dev/`（本地调试状态/日志/测试连接数据，可能含内网信息）、`.todo/`（本地待办库）、`tools/ptyserver/pty-modes.json`（测试运行时产物）、`node_modules/`、`ui/dist/`、`backend/bin/`、所有 `*.exe`、`*.log`、`.dbx-repository-signing-key.env`（签名密钥，绝不能传）。
- 发布流程已就绪：在 GitHub 上创建 Release 并打 tag，Actions 会自动执行 `dbx-plugin package` 并把 `.dbxp` 产物挂到 Release。

---

## 七、优化方向深挖（2026-10-28 补充；v0.5.0 已批量落地）

前面的路线图回答"做什么功能"，这一节回答"把已有的东西做得更快、更稳、更省"。按感知收益排序。

### A. 传输性能（用户感知最强）

1. ✅ **下载管道化（v0.5.0）**：SFTP 文件句柄实现 `ReadAt`，大文件下载走 `pumpDownloadAt`——4 个并发 stripe 读 + 严格按偏移顺序发送，ack 流控预算与顺序泵一致（`manager.go`）。小文件/普通 reader 保持顺序泵。e2e 以 1MB 多 stripe 下载字节级一致验证。
2. ✅ **zip 打包流式化（v0.5.0）**：`StartDownloadZip` 改为 `io.Pipe` 边打包边推流（`zipInto`）——省一倍磁盘 IO、零临时文件，第一个字节即开始下载；打包中的任何错误经管道传给 pump，在 `transfer.done` 报失败（顺序泵同时改为"仅 io.EOF 算完成"）。
3. **多文件并行**：批量传输目前逐文件串行，可按文件级并行（限制并发数防占满带宽）。→ 与 UI 传输队列联动，留待 UI 侧一起做。
4. **断点续传**：下载写 `.part` 临时文件 + 记录 offset，失败重试从断点继续；需要协议与 UI 保存句柄配合，留作传输批次 v2。

### B. 终端渲染与交互

1. ✅ **WebGL 渲染器（v0.5.0）**：term.ts 加载 `@xterm/addon-webgl`（`onContextLoss` 自动降级 + try/catch 回退默认渲染器），刷屏场景走 GPU。
2. ✅ **输出合帧（v0.5.0）**：`pumpOutput` 改为 reader goroutine + 4ms 微批窗口（32KB 强制刷），高吞吐输出桥帧数下降一个量级；ring 与宿主字节流不变。
3. **回放瘦身**：attach 回放是 128KB 环形缓冲全量推送——可改为"尾部 N 屏 + 上划时按需向前拉取"（需协议扩展，留待后续）。
4. ✅ **addon 补齐（v0.5.0）**：search（Ctrl+F 自带悬浮搜索栏，Enter/Shift+Enter 前后查找，Esc 关闭）、web-links（链接可点），全部自包含在 term.ts。

### C. 连接层

1. ✅ **私钥签名器缓存（v0.5.0）**：`cachedSigner` 按 (路径, mtime, 口令摘要) 缓存 `ssh.Signer`（`shell.go`），重拨不再重复读盘解析；密钥文件更新后 mtime 变化自动重载。
2. ✅ **SFTP 自愈（v0.5.0）**：`Conn.SFTP()` 每次调用用 `Stat(".")` 探活，传输层死亡（EOF/connection lost）自动关旧建新，文件面板无感恢复；同时开启 `UseConcurrentReads`。
3. ✅ **超时可配（v0.5.0）**：连接表单新增"连接超时(秒)"（`connect_timeout`，默认 20，钳制 5..120），`dialSSH` 从 spec 读取。

### D. 工程质量（开源前值得做）

1. ✅ **CI 门禁（v0.5.0）**：`.github/workflows/ci.yml`（windows-latest）——vet + 单测 + 构建 + UI 构建 + check + proto + e2e 全链路。
2. **`go test -race`**：本机可跑；CI 待确认 windows runner 的 cgo 矩阵后再加。
3. ✅ **版本一致性校验（v0.5.0）**：`scripts/check-version.mjs` 对账 manifest.json 与 main.go（根治"改一半"事故）。
4. ✅ **tsc --noEmit 门禁（v0.5.0）**：`npm run check` 串联版本一致性 + 类型检查 + 构建断言。
5. ✅ **构建瘦身（v0.5.0）**：`-ldflags "-s -w"`（本地直接传给 go build；`dbx-plugin package` 用 `GOFLAGS="-ldflags=-s -w"` 注入）——sidecar 9.4MB→6.5MB，.dbxp 5.5MB→2.9MB。CI 已内置。
   另：构建断言脚本 `scripts/check-build.mjs`（5 个关键 CSS 标记，防 style.css 丢失复发）也随 check 门禁运行。

### E. 资源占用

1. ✅ **ring 缓冲惰性分配**：`bytes.Buffer` 零值本就惰性分配，确认无固定占用——无需改动。
2. ✅ **传输内存复用（v0.5.0）**：下载 chunk 走 `chunkBufPool`（sync.Pool）；SDK `Emitter.Binary` 已证实同步拷贝，缓冲在发送返回后立即可复用。
3. **diag.log 升级**：JSON 行格式 + 级别过滤（现格式够用，优先级低）。

**v0.5.0 遗留**：A3 多文件并行、A4 断点续传、B3 回放瘦身、E3 diag 结构化——都需要协议扩展或与 UI 传输队列联动，归入传输批次 v2；D2 (-race) 待 CI 验证。

