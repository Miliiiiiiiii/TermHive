# Changelog

本文件记录每个**发布版本**的变更。版本规则见 [docs/VERSIONING.md](docs/VERSIONING.md)。

## [0.1.0] - unreleased

首个版本:SSH 终端 + SFTP 文件管理的 DBX 插件,连接保存在 DBX 原生连接列表。

### 连接与安全

- DBX 原生 connection-provider:密码/私钥/键盘交互认证、认证方式智能回退、用户名密码防呆校验;keepalive、GBK/GB18030 编码、连接超时与登录后命令
- 服务器指纹校验(TOFU,known_hosts.json):指纹变更拒绝连接并给出告警与重置方法
- 重连凭据密封落盘(Windows DPAPI / 其他平台 AES-256-GCM),明文只存在于内存

### 终端

- 每会话独立 xterm 实例;WebGL 渲染(自动降级)、Ctrl+F 搜索、链接可点、输出微批合帧
- 断线"重新连接"覆盖层;历史输出环形缓冲(128KB)attach 回放
- 浮窗迷你终端(桌面宿主 `floating` API)

### 多会话与分组

- 同一连接任意多会话,全部复用一条 SSH 连接;标签名持久、双击重命名
- 会话分组:拖拽/右键建组,组内自动分栏(≤4 栏:2/3 纵排、4 栏 2×2),跨服务器可同组,点组内任一标签整组上屏

### 文件管理

- SFTP 浏览/上传/下载/重命名/删除/新建文件夹;拖拽上传(含文件夹递归)
- 多选批量下载:zip 流式打包,边打包边下载
- 大文件下载并发分块读(按序重组)、下载缓冲池化;SFTP 通道断线自愈
- 文件面板跟随终端 cwd(OSC 7 shell 集成,会话级开关)
- 远程文件编辑:连接编码自动转码、≤2MB 全量可编辑、超过分段只读浏览、二进制识别、临时文件 + 原子替换保存(Ctrl+S)

### AI 工具(MCP)

- 9 个工具分三档对外授权:`read`(只读,零授权即暴露)/ `write`(SFTP 写)/ `full`(exec 任意命令),供 DBX 内置 AI 与外部 agent 使用
- 安全分层:白名单命令分类器(禁重定向/命令替换、排除高危解释器)、灾难模式硬拒(`rm -rf /`、`mkfs`、fork 炸弹等)、写操作逐次弹窗批准、SFTP 路径护栏、命令超时与输出上限、调用进 diag.log 与 DBX 审计

### 设置与诊断

- 插件设置页:配色主题(One Dark / Dracula / Solarized Dark / Nord / GitHub Light / 跟随 DBX)+ 日志着色规则(正则 + 7 色 + 整行/片段,完全自定义),插件级实时广播到所有已打开终端与浮窗
- 飞行记录仪:全量 UI↔后端调用写 diag.log(自动脱敏、封顶 128KiB)

### 工程

- proto / e2e / PTY 模式测试套件(内嵌 SSH+SFTP 测试服务器)
- CI 门禁:版本一致性 + CHANGELOG 覆盖 + 类型检查 + 构建断言 + 全量测试
- 发版脚本(release:patch/minor)+ GitHub Actions 自动打包 .dbxp 挂到 Release
