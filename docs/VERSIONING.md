# 版本管理与发布流程

## 原则

- **版本号只在发布点前进**:开发过程中的日常改动不动版本号。版本号代表"对外发布的批次",不是"第几次编译"。
- **版本号变更以维护者明确指令为唯一触发**:认为需要新的版本号(正式号或 rc 预发布号)时,先说明理由,经确认后再动,不自行升号。

## 版本号语义(semver)

`MAJOR.MINOR.PATCH`,当前处于 `0.x` 开发期:

| 段 | 何时前进 | 示例 |
|---|---|---|
| MINOR | 一个**规划好的功能批次**发布(先在 [ROADMAP](ROADMAP.md) 立项) | 0.1.0 → 0.2.0(日志模式批次) |
| PATCH | 一批**修复/小调整**发布 | 0.1.0 → 0.1.1 |
| MAJOR | 1.0.0:功能集与数据格式稳定 | — |

## 中间验证构建(rc 预发布号)

开发中途需要真机验证时,用预发布号,不占用正式版本号:

```
0.2.0-rc.1  →  0.2.0-rc.2  →  …  →  0.2.0(正式发布)
```

- rc 号只在同一目标版本内递增;正式发布落回无后缀号;
- CHANGELOG 里 rc 构建只在对应正式版本条目下记一行,不单独成条;
- 打 rc 号同样是版本号变更,同样需要维护者确认;
- 命令:`npm run release:rc`(0.1.0 → 0.1.0-rc.1 → 0.1.0-rc.2 …,自动在对应版本条目下记一行验证构建);该版本自测定稿、正式发布时用 `npm run release:settle` 去掉 rc 后缀落回正式号。

## 发版流程(唯一合法的版本变更方式)

```bash
# 1. 在 CHANGELOG.md 写好下一版的 ## [x.y.z] 条目(脚本会强制校验存在)
# 2. 执行发版:
npm run release:patch   # 或 release:minor;若当前带 rc 后缀,先 npm run release:settle 落回正式号
# 3. 脚本做的事:校验 CHANGELOG 有对应条目 → 一致地升 manifest.json 与 backend/main.go → 输出核对清单
# 4. 人工执行:npm test && npm run package
# 5. 打 git tag x.y.z 并推送,在 GitHub 建 Release(Actions 自动打包 .dbxp 挂到 Release)
```

## 门禁(防事故)

`npm run check`(CI 同样执行)包含两项硬校验:

1. `check-version.mjs`:manifest.json 与 backend/main.go 的版本必须一致;
2. `check-changelog.mjs`:CHANGELOG.md 必须存在**当前版本**的条目——版本前进而无变更记录,直接失败。

## 与 DBX 平台约束的关系

DBX 按版本号目录安装插件(`plugins/<id>/versions/<ver>/`)且逐文件 sha256 校验——"改了代码但版本没变"的包无法体现为新版本。因此**只有发版才打安装包**:日常开发在本地测试闭环(e2e/proto/单测),需要真机验证时用 rc 号,正式包只在发布点打。
