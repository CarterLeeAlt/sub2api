# Fork 维护记录（自 GitHub Fork 创建点起）

本文档记录本仓库相对上游的长期定制、上游同步节点和已知冲突处理方式。它用于回答“这个 fork 为什么与上游不同”，不代替 Git 提交历史或上游的发布日志。

## 仓库关系

| 项目 | 地址或节点 |
| --- | --- |
| 本仓库（`origin`） | <https://github.com/CarterLeeAlt/sub2api> |
| 上游仓库（`upstream`） | <https://github.com/Wei-Shaw/sub2api> |
| 维护分支 | `main` |
| GitHub Fork 创建时间 | 2026-08-09 17:14:19 UTC（北京时间 2026-08-10 01:14:19） |
| Fork 创建时的上游节点 | [`48eb3766`](https://github.com/Wei-Shaw/sub2api/commit/48eb3766d2da817b171b45bb3036d42575e42b8f)（`v0.1.173`） |
| 当前已同步上游节点 | [`baeac1f3d`](https://github.com/Wei-Shaw/sub2api/commit/baeac1f3de21d37b129405f092ef86c24b3f203d) |
| 最近一次上游合并提交 | [`78fe75aa0`](https://github.com/CarterLeeAlt/sub2api/commit/78fe75aa0d5d14f6634ac9bd521ab640ad019248) |

上游更新使用普通 merge 合入 `main`，保留 merge commit，不采用 squash 或 rebase。这样可以明确区分上游历史与 fork 自有提交，也便于在下一次同步时定位共同祖先。

## 状态定义

| 状态 | 含义 |
| --- | --- |
| `active` | fork 当前仍依赖的本地能力；同步上游时必须检查并保留，除非上游已有等价实现。 |
| `upstreamed` | 功能已由上游提供；本地可能仅保留覆盖更完整的测试。 |
| `retired` | 一次性迁移或临时机制已经完成并删除，不应在后续合并中恢复。 |

## 当前定制

### CUSTOM-001：OpenAI OAuth 模型清单同步（`active`）

OpenAI OAuth 账户从 Codex models manifest 获取实时模型清单，而不是依赖 API Key 账户使用的 `/v1/models`。同步时：

- 排除 `supported_in_api=false` 的模型；
- 对模型 ID 去重并排序；
- 将上游清单作为权威来源，替换旧白名单，而不是只追加新模型；
- 账户满足套餐和图像能力条件时，补充虚拟模型 `gpt-image-2`，供现有生图入口使用；
- 前端账户模型白名单选择器允许展示和保存同步结果。

主要文件：

- `backend/internal/service/upstream_models.go`
- `backend/internal/service/upstream_models_openai_oauth.go`
- `backend/internal/service/upstream_models_openai_oauth_test.go`
- `frontend/src/components/account/ModelWhitelistSelector.vue`

相关提交：[`e7773796`](https://github.com/CarterLeeAlt/sub2api/commit/e77737963b3f66a961dcb9ebd2dc428023fe5143)、[`ec4e3bc9`](https://github.com/CarterLeeAlt/sub2api/commit/ec4e3bc9cb4075b40b4c21a5b5b48538cc03a22f)、[`12d73937`](https://github.com/CarterLeeAlt/sub2api/commit/12d7393714d5750f13114e5b150173400abdf7bf)、[`bf11d250`](https://github.com/CarterLeeAlt/sub2api/commit/bf11d250908d1308bea8a10c8f323927c7b02da1)。

### CUSTOM-002：OpenAI OAuth 动态生图主模型（`active`）

生图能力不再绑定固定的 `gpt-5.4-mini`。系统根据 Codex manifest 为当前 OAuth 账户选择支持图像输入的 Responses 主模型：

- manifest 中存在 `gpt-5.4-mini` 且可用时仍优先选择它；
- 否则按 manifest 的 `priority` 选择可用模型，并用模型名保证同优先级下结果稳定；
- 缺少 `input_modalities` 时沿用 Codex 的兼容语义，视为支持文本和图像；
- `free` 或无法确定套餐的账户不暴露 `gpt-image-2`；
- manifest 临时获取或解析失败时回退到历史主模型，明确确认没有可用图像模型时返回错误；
- `gpt-image-2` 仍是对外工具模型，动态选择的是调用 `image_generation` 工具的顶层 Responses 模型。

主要文件：

- `backend/internal/service/upstream_models_openai_oauth.go`
- `backend/internal/service/openai_images_oauth_model_selection.go`
- `backend/internal/service/openai_images_responses.go`
- `backend/internal/service/upstream_models_openai_oauth_test.go`

相关提交：[`29f5d98e`](https://github.com/CarterLeeAlt/sub2api/commit/29f5d98e9f8060995a7ec3febf073e35ba6e0589)、[`527b3aa5`](https://github.com/CarterLeeAlt/sub2api/commit/527b3aa55b1dfbb4575bd25c8d39ad8e0b090ab4)、[`a2829c13`](https://github.com/CarterLeeAlt/sub2api/commit/a2829c138abcbbb60b9e858d207b7a43fbfdc2aa)、[`abd725ec`](https://github.com/CarterLeeAlt/sub2api/commit/abd725ece1170f3acf831be8a8d7af3c0bc55949)。

### CUSTOM-003：自有 GHCR 镜像与回归门禁（`active`）

每次向 `main` 推送或手动触发工作流时，构建并推送 `ghcr.io/carterleealt/sub2api`：

- 发布 `latest` 和固定 7 位的 `sha-xxxxxxx` 标签；
- 构建前运行本 fork 的 OpenAI 定向回归测试；
- 仅在回归测试通过后构建 `linux/amd64` 镜像；
- 将完整 Git commit 写入镜像标签、OCI revision 和前后端构建信息。
- Docker 后端构建镜像的 Go 版本必须与 `backend/go.mod` 声明严格一致；当前两者均为 `1.26.6`。

主要文件：

- `.github/workflows/custom-docker.yml`
- `Dockerfile`

相关提交：[`40a7694b`](https://github.com/CarterLeeAlt/sub2api/commit/40a7694b2ce8cb9aadf18b340d1a875f253de6f8)、[`99717aa1`](https://github.com/CarterLeeAlt/sub2api/commit/99717aa1d100cb94da8945589dc86e2be50a2a47)、[`5914e9fef`](https://github.com/CarterLeeAlt/sub2api/commit/5914e9fef6e70c493c4318553102a254e72e76c1)。

### CUSTOM-004：界面显示镜像构建标识（`active`）

镜像继续使用与 Git 提交一致的 `sha-xxxxxxx` 构建标识，但左上角收起状态只显示大版本号，例如 `v0.1.175`，避免 SHA 干扰日常界面阅读。点击版本徽章展开下拉详情后仍显示 `sha-xxxxxxx`，用于确认正在运行的容器是否来自预期构建；该徽章不负责自动检查或拉取镜像更新。

主要文件：

- `Dockerfile`
- `frontend/src/components/common/VersionBadge.vue`
- `frontend/src/utils/buildIdentity.ts`
- `frontend/src/utils/__tests__/buildIdentity.spec.ts`
- `frontend/src/vite-env.d.ts`

相关提交：[`3e72a088`](https://github.com/CarterLeeAlt/sub2api/commit/3e72a088264568f3d744a60e45be246a82a4e9dc)、[`5306cbe37`](https://github.com/CarterLeeAlt/sub2api/commit/5306cbe374b0ffffe19d66690baf21851d0d8959)。

### CUSTOM-005：内容审核缓存测试稳定化（`active`，仅测试）

`TestContentModerationRuntimeSnapshotRefreshFailureKeepsStaleConfig` 不再使用 `1ns` TTL 假设两次相邻的 Windows 时钟读取必然不同。测试改为显式把运行时快照设为过期，保持“异步刷新失败时继续使用旧配置”的原始验证目标，同时消除与 Windows 时钟粒度相关的不稳定失败。

主要文件：

- `backend/internal/service/content_moderation_runtime_cache_test.go`

相关提交：[`9f5bde85`](https://github.com/CarterLeeAlt/sub2api/commit/9f5bde85c49185826ffb4c512e9612a412c81fcf)。

### CUSTOM-006：Codex 账号级停调阈值统一与状态协调（`active`）

Codex/OpenAI 账户的通用平台阈值 `credentials.account_scheduling_threshold` 现在是账号级最高优先级策略：

- 账号设置 `1-99` 时，该值统一接管 5h 与 7d 两个配额窗口；任一窗口达到已用百分比阈值即停止调度；
- 账号设置 `100` 时，禁用该账号的全部配额自动停调；
- 旧版 `extra.auto_pause_5h_*`、`extra.auto_pause_7d_*` 与运维页 Codex 默认值只在账号未设置通用覆盖时生效；
- 编辑阈值后立即重新评估已有的 `account_scheduling_threshold` 停调原因，并以 compare-and-swap 方式协调数据库、Redis、调度快照和运行时快速阻断；其他停调原因及模型级限流不受影响；
- 进程启动时在数据库中筛选仍带结构化阈值停调原因的活动账号并按 ID 分页协调；单个 worker 最多执行三次独立超时的指数退避重试，避免启动瞬时故障被 `sync.Once` 永久固化；
- WHAM 用量刷新按响应完成时间生成高精度 `codex_wham_usage_updated_at`，先同步、单调写入数据库和 scheduler outbox，再以“旧停调原因 + 精确 WHAM 代际”双重 CAS 恢复；持久化失败、旧响应晚到、代际变化或协调器缺失时保持原停调状态；
- `/wham/usage` 的 HTTP 200 将 `rate_limit` 显式 `null` 视为权威无窗口，将字段完全缺失视为不完整响应；不完整响应保留上一份可靠快照且不参与本次恢复；
- 调度列表使用的 `sched:meta:<id>` 精简快照保留账号阈值覆盖值、账号版本以及各平台阈值判断所需的窗口字段，避免账号覆盖为 `100` 时仍被精简快照按全局阈值反复停调；
- Codex/Anthropic 调度判断依赖的额度字段与数据库更新原子写入耐久 outbox；事件沿用 `account_changed` 并携带 `metadata_only=true`，新 worker 只刷新账号元数据、不重建分组 bucket，旧 worker 则安全执行完整刷新，支持滚动升级；
- 新增阈值停调写入会重新读取当前账号、重新评估并以 `updated_at` compare-and-swap 原子写入数据库和 scheduler outbox；CAS 成功后才发布 Redis/运行时阻断，账号保存与调度写入竞态不再复活旧阈值状态；
- 账户编辑界面启用通用覆盖时禁用旧版 Codex 配额自动暂停控件，明确显示优先级；覆盖启用控件与同页其他布尔项统一使用开关样式。
- 同一账号的后台用量/状态刷新不再重置已打开的编辑表单；账号保存会使保存前发出的批量用量请求失效，迟到的 `account_updates` 不再回滚刚保存的阈值开关或账号对象。

主要文件：

- `backend/internal/repository/account_repo.go`
- `backend/internal/repository/scheduler_cache.go`
- `backend/internal/service/account_scheduling_threshold_eval.go`
- `backend/internal/service/account_usage_service.go`
- `backend/internal/service/openai_quota_service.go`
- `backend/internal/service/openai_gateway_scheduling.go`
- `backend/internal/service/ratelimit_service.go`
- `backend/internal/service/scheduler_events.go`
- `backend/internal/service/scheduler_snapshot_service.go`
- `backend/internal/service/wire.go`
- `backend/cmd/server/wire_gen.go`
- `frontend/src/components/account/EditAccountModal.vue`
- `frontend/src/components/account/__tests__/EditAccountModal.spec.ts`
- `frontend/src/views/admin/AccountsView.vue`
- `frontend/src/views/admin/__tests__/AccountsView.usageWindowsHint.spec.ts`

相关提交：[`23eab2e32`](https://github.com/CarterLeeAlt/sub2api/commit/23eab2e32c5a8db90feb385a0f9ce30abc426557)、[`be0367c37`](https://github.com/CarterLeeAlt/sub2api/commit/be0367c37c9ab566eb5ee5517d1508b5dc9e71b6)、[`c9c28fc8a`](https://github.com/CarterLeeAlt/sub2api/commit/c9c28fc8a1aef9705a162d39181ec17c2e8aa91b)、[`36ad5b5ee`](https://github.com/CarterLeeAlt/sub2api/commit/36ad5b5eec735c90dee5bfa52ac8870b835091ff)、[`7b233dec2`](https://github.com/CarterLeeAlt/sub2api/commit/7b233dec29d4b8902ea6de4ebb75be0864bab94e)、[`e7d474548`](https://github.com/CarterLeeAlt/sub2api/commit/e7d47454859c0ff3b618a2497f8e6246df6c1cf3)。

验证：后端完整单元测试 `go test -tags unit ./... -count=1`、静态检查 `go vet ./...` 和服务端离线构建均通过。

### CUSTOM-007：Codex 指纹请求起始时间一致性（`active`）

Codex 指纹的 `turn_started_at_unix_ms` 在解析一次请求的指纹 ID 时生成一次，随后由 HTTP 头、普通 JSON 请求体和 raw 透传请求体共享。不得在各载体改写函数中分别读取当前时间，否则同一请求可能出现 1 毫秒差异。

主要文件：

- `backend/internal/service/openai_codex_fingerprint.go`
- `backend/internal/service/openai_codex_fingerprint_test.go`

相关提交：[`992b51eb7`](https://github.com/CarterLeeAlt/sub2api/commit/992b51eb7d601bcb2fb490f185b45722d08dfcaa)。

### CUSTOM-008：分组用量汇总时区测试稳定化（`active`，仅测试）

分组用量汇总触发器集成测试显式设置数据库会话时区，避免运行环境默认时区不同导致日期边界断言漂移。

主要文件：

- `backend/internal/repository/group_usage_rollup_trigger_integration_test.go`

相关提交：[`adaf4ed3d`](https://github.com/CarterLeeAlt/sub2api/commit/adaf4ed3d86b287bb3cda96a879a342bfa7a2c2b)。

### CUSTOM-009：表格仅允许显式复选框选择（`active`）

账号管理和代理管理不启用鼠标拖拽框选，避免用户拖动页面或复制内容时误选记录。单项选择统一通过每行左侧复选框完成，批量选择保留表头全选框和页面已有的明确批量选择控件。

主要文件：

- `frontend/src/views/admin/AccountsView.vue`
- `frontend/src/views/admin/ProxiesView.vue`
- `frontend/src/composables/useSwipeSelect.ts`（已删除，后续同步不得恢复）

相关提交：[`96bf166d5`](https://github.com/CarterLeeAlt/sub2api/commit/96bf166d5f416b3e7aa586a71c0a0fcecf73ca1e)。

### CUSTOM-010：代理管理中文命名统一（`active`）

中文导航、页面标题及面向用户的功能提示统一使用“代理管理”，不再使用容易误解为单纯 IP 地址维护的旧称；英文引用同步使用“Proxy Management”。路由、接口和代码中的 `proxy` / `proxies` 技术标识保持不变。

主要文件：

- `frontend/src/i18n/locales/zh/common.ts`
- `frontend/src/i18n/locales/zh/admin/resources.ts`
- `frontend/src/i18n/locales/zh/admin/channels.ts`
- `frontend/src/i18n/locales/en/admin/channels.ts`

相关提交：[`96bf166d5`](https://github.com/CarterLeeAlt/sub2api/commit/96bf166d5f416b3e7aa586a71c0a0fcecf73ca1e)。

## 已被上游吸收

### OpenAI 调度阈值百分比语义（`upstreamed`）

fork 最初修正了 Codex 调度用量的单位，确保阈值比较使用百分比而不是小数比例。同步到上游节点 `10a4c6e3` 时，上游已通过 PR `#5468` 提供等价生产实现，因此不再把生产代码视为 fork 独有功能。

本地保留了覆盖 OpenAI 和 Anthropic 百分比语义的表驱动测试，作为后续上游合并的回归保护。

相关提交：[`d39b99a3`](https://github.com/CarterLeeAlt/sub2api/commit/d39b99a3bd9339882a0c0c6b9eccea7e45aa52dd)、[`2e61f7d4`](https://github.com/CarterLeeAlt/sub2api/commit/2e61f7d4a767e8ad833103c26e853da43b2107c4)。

## 已退役机制

### Codex 5h/7d 动态窗口显隐（`retired`）

提交 `2e6b9378` 引入了根据上游 Codex 窗口存在性自动显示或隐藏 5h/7d 用量行的机制，后续由 `d972d6ea`、`a5a8e72a` 和 `c8d37d22` 补充迁移、三次缺失确认及 OAuth/PAT 探测。由于上游响应不能稳定、权威地表示窗口缺失，该机制已整体回退到引入前的固定窗口行为。

回退后：

- 不再写入或读取 `codex_5h_window_present`、`codex_7d_window_present` 及缺失计数；
- 不再因连续缺失自动删除旧的 5h/7d 快照，也不再为窗口恢复定期探测；
- 恢复原有的 5h/7d 固定显示与本地用量汇总逻辑，不改变计费窗口的时间起点、重置时间或费用统计语义；
- 数据库中已有的 presence/missing-count 字段保留为无效兼容数据，无需迁移或清理。

后续上游合并不得恢复该动态显隐机制，除非有单一、稳定且权威的上游配额窗口来源，并重新经过独立评估。

相关提交：[`2e6b9378`](https://github.com/CarterLeeAlt/sub2api/commit/2e6b937801e84358b63808c74eae41f05d493b6a)、[`d972d6ea`](https://github.com/CarterLeeAlt/sub2api/commit/d972d6ea8129ecddb0dd251a83a567dba5e2d71c)、[`a5a8e72a`](https://github.com/CarterLeeAlt/sub2api/commit/a5a8e72a0cd21954a95fa51f814fc2cde6861301)、[`c8d37d22`](https://github.com/CarterLeeAlt/sub2api/commit/c8d37d2227920072ff2cecde9fd61dca0fcd81ea)。

### 一次性生图主模型迁移工作流（`retired`）

提交 `c3031d0e` 曾加入一次性 GitHub Actions 工作流，用于把动态生图主模型改动应用到源码；随后由 `abd725ec` 落地实际源码变更，并在 `5791cb14` 删除该工作流。

后续合并不得恢复已删除的一次性工作流。动态生图实现本身仍属于 `CUSTOM-002`，继续保留。

相关提交：[`c3031d0e`](https://github.com/CarterLeeAlt/sub2api/commit/c3031d0ef726af217307639afd270df71097ab4d)、[`abd725ec`](https://github.com/CarterLeeAlt/sub2api/commit/abd725ece1170f3acf831be8a8d7af3c0bc55949)、[`5791cb14`](https://github.com/CarterLeeAlt/sub2api/commit/5791cb1449ace7ce136e1fd3192fb9d8294b5585)。

## 已知上游合并处理

### 2026-08-16：同步至上游 `baeac1f3d`

合并提交：[`78fe75aa0`](https://github.com/CarterLeeAlt/sub2api/commit/78fe75aa0d5d14f6634ac9bd521ab640ad019248)。

本次同步将上游版本推进到 `v0.1.177`，合入分组用量日汇总与时区迁移、Codex turn state/compaction、OpenAI channel restriction、Responses HTTP/WS 路径增强以及相应后端和前端测试。合并保留了 fork 的 OAuth manifest 模型清单、动态生图主模型、自有 GHCR 工作流、版本徽章、手动更新策略、额度百分比语义和界面定制；Git 合并未产生需要手工解决的文本冲突。

合并后的补充修正包括：Docker Go 构建镜像对齐 `backend/go.mod` 的 `1.26.6`、分组汇总集成测试固定数据库时区，以及账号恢复和账号级 Codex 停调阈值的状态协调。这些后续提交均列入下方提交索引和对应 `CUSTOM` 条目。

### 2026-08-13：同步至上游 `fbfdcef81`

合并提交：[`5306cbe37`](https://github.com/CarterLeeAlt/sub2api/commit/5306cbe374b0ffffe19d66690baf21851d0d8959)。

本次上游 `main` 相对共同祖先 `5935e674` 新增 26 个提交；本地 fork 同时保留 35 个独有提交。Git 自动合并无文本冲突，但以下 4 个文件发生语义重叠，按功能逐段合并：

- `backend/internal/service/openai_gateway_usage.go`：保留本地 OpenAI OAuth 动态图片模型和 Codex 用量逻辑，同时接入上游 Group → Channel → 内置的逐模型媒体定价、长上下文开关和 Grok 计费路径；未采用整文件 ours/theirs 覆盖。
- `frontend/src/components/account/AccountUsageCell.vue`：保留本地 OpenAI 用量剩余百分比显示，并接入上游 Grok 免费/付费档位判断。
- `frontend/src/components/account/__tests__/AccountUsageCell.spec.ts`：保留本地 Codex 用量断言，同时合并上游 Grok 快照、Free 和 Lite 覆盖。
- `frontend/src/views/admin/AccountsView.vue`：保留本地账户页布局修改，并接入上游 Grok 用量快照增量刷新和订阅档位展示。

本次同步还保留 fork 的 OAuth manifest 模型清单同步、动态生图主模型选择、自有 GHCR 工作流、手动 Docker 更新策略、字体和账户管理界面定制。后端 `internal/service` 单元测试、受影响前端测试、TypeScript 类型检查和生产构建均通过。

本次新增的版本徽章行为：左上角常驻区域仅显示 `v0.1.175` 形式的版本号，`sha-xxxxxxx` 仅在点击后的下拉详情中显示；对应测试位于 `frontend/src/components/common/__tests__/VersionBadge.spec.ts`。

### 2026-08-12：同步至上游 `5935e674`

合并提交：[`810eef47`](https://github.com/CarterLeeAlt/sub2api/commit/810eef477fee4303645b8d04eda21785dd919ed5)。

本次上游改动与 fork 定制路径没有文本冲突，直接合并后按语义复核：

- 接受上游 Codex 指纹收敛功能，并保持未设置或非法配置时默认使用 `session`；只有显式配置 `off` 才关闭；
- 保留 `CUSTOM-001` 的 OAuth manifest 模型同步及 `CUSTOM-002` 的动态生图主模型选择，现有 fork 测试未被上游重复测试替换；
- 确认常规 Codex 转发会共享同一组指纹 ID 改写请求体和请求头；manifest 获取与自定义 `/v1/images` 路径继续使用各自原有流程；
- 同时合入 Responses HTTP/WS v2 可见 TTFT 修复、HTML 403 免惩罚故障转移、嵌套 usage 解析、`service_tier` 计费和版本 `v0.1.175`；
- 修正指纹代码中把 `off` 描述为默认行为的过时注释，使其与实际默认 `session` 一致；
- Codex/OpenAI 定向 Go 测试、完整 `internal/service` 单元测试、8 个受影响前端测试文件共 114 项测试、类型检查和生产构建均通过。

### 2026-08-12：改为手动 Docker 更新策略

为避免上游的在线二进制更新机制覆盖 fork 定制，版本徽标现在只检查并提示上游 Release 是否有新版，不再提供在线更新、服务重启或版本回退入口。后端更新、回退和回退版本接口默认返回 `403`，Docker 镜像的拉取、重建和回退由维护者在宿主机手动执行。

### 2026-08-11：同步至上游 `1e618dbc`

合并提交：[`9f5bde85`](https://github.com/CarterLeeAlt/sub2api/commit/9f5bde85c49185826ffb4c512e9612a412c81fcf)。

唯一的文本冲突仍位于 `backend/internal/service/account_scheduling_threshold_eval_test.go`。处理结果是：

- 保留本地覆盖两个窗口和边界值的 OpenAI 表驱动百分比测试；
- 保留本地 Anthropic 百分比语义测试，删除上游语义重复的两个简单测试；
- 保留上游新增的 4 个陈旧、已重置和新鲜 Codex 快照测试；
- 将本地测试调用适配到上游新增的 `now` 参数；
- `backend/internal/service/openai_images_responses.go` 自动合并，确认本地动态主模型选择与上游流读取错误故障转移同时保留；
- 稳定化内容审核缓存测试，消除 Windows `1ns` TTL 时序假设；
- 定向 Go 测试、完整 `internal/service` 单元测试、39 项受影响前端测试、类型检查和生产构建均通过。

### 2026-08-10：同步至上游 `10a4c6e3`

合并提交：[`57a1f29f`](https://github.com/CarterLeeAlt/sub2api/commit/57a1f29fc6c71168d6a3a092b4a4611c4e3c58ad)。

唯一的文本冲突位于 `backend/internal/service/account_scheduling_threshold_eval_test.go`。处理结果是：

- 保留本地覆盖 OpenAI 与 Anthropic 的表驱动百分比测试；
- 删除上游语义重复、覆盖范围较窄的测试；
- 接受上游生产实现，因为它与本地阈值修复等价；
- 其余上游变更直接合入。

## 从 GitHub Fork 创建点开始的提交索引

GitHub 仓库元数据中的 `created_at` 为 `2026-08-09T17:14:19Z`。按该时间查询上游 `main`，当时的最新节点为 `48eb3766`。以下记录从这个实际 fork 创建节点开始，并按本仓库 `main` 的 first-parent 历史排列。纯文档维护提交不加入此索引，避免记录文件自身造成无意义的递归更新。

| 顺序 | 提交 | 类型 | 结果 |
| ---: | --- | --- | --- |
| 0 | [`48eb3766`](https://github.com/Wei-Shaw/sub2api/commit/48eb3766d2da817b171b45bb3036d42575e42b8f) | GitHub fork 创建点 | 创建 fork 时上游 `main` 的最新提交，上游版本 `v0.1.173`。 |
| 1 | [`d39b99a3`](https://github.com/CarterLeeAlt/sub2api/commit/d39b99a3bd9339882a0c0c6b9eccea7e45aa52dd) | 修复 | 调度用量统一使用百分比语义；生产改动后来被上游吸收。 |
| 2 | [`2e61f7d4`](https://github.com/CarterLeeAlt/sub2api/commit/2e61f7d4a767e8ad833103c26e853da43b2107c4) | 测试 | 增加 OpenAI 与 Anthropic 调度阈值回归覆盖，当前保留。 |
| 3 | [`40a7694b`](https://github.com/CarterLeeAlt/sub2api/commit/40a7694b2ce8cb9aadf18b340d1a875f253de6f8) | CI | 从 `main` 构建 fork 自有 GHCR 镜像。 |
| 4 | [`e7773796`](https://github.com/CarterLeeAlt/sub2api/commit/e77737963b3f66a961dcb9ebd2dc428023fe5143) | 修复 | 从 Codex manifest 同步 OpenAI OAuth 模型。 |
| 5 | [`ec4e3bc9`](https://github.com/CarterLeeAlt/sub2api/commit/ec4e3bc9cb4075b40b4c21a5b5b48538cc03a22f) | 修复 | 只同步 API 可用模型。 |
| 6 | [`12d73937`](https://github.com/CarterLeeAlt/sub2api/commit/12d7393714d5750f13114e5b150173400abdf7bf) | 测试 | 覆盖 OAuth 模型能力同步。 |
| 7 | [`bf11d250`](https://github.com/CarterLeeAlt/sub2api/commit/bf11d250908d1308bea8a10c8f323927c7b02da1) | 修复 | 将 OAuth manifest 设为权威模型清单。 |
| 8 | [`29f5d98e`](https://github.com/CarterLeeAlt/sub2api/commit/29f5d98e9f8060995a7ec3febf073e35ba6e0589) | 修复 | 生图资格不再依赖固定主模型。 |
| 9 | [`527b3aa5`](https://github.com/CarterLeeAlt/sub2api/commit/527b3aa55b1dfbb4575bd25c8d39ad8e0b090ab4) | 功能 | 从 manifest 动态选择 OAuth 生图主模型。 |
| 10 | [`a2829c13`](https://github.com/CarterLeeAlt/sub2api/commit/a2829c138abcbbb60b9e858d207b7a43fbfdc2aa) | 测试 | 覆盖动态 OAuth 生图主模型选择。 |
| 11 | [`c3031d0e`](https://github.com/CarterLeeAlt/sub2api/commit/c3031d0ef726af217307639afd270df71097ab4d) | 临时 CI | 添加一次性源码迁移工作流，现已退役。 |
| 12 | [`abd725ec`](https://github.com/CarterLeeAlt/sub2api/commit/abd725ece1170f3acf831be8a8d7af3c0bc55949) | 修复 | 将动态 Codex 主模型接入实际生图请求。 |
| 13 | [`5791cb14`](https://github.com/CarterLeeAlt/sub2api/commit/5791cb1449ace7ce136e1fd3192fb9d8294b5585) | CI | 删除已完成任务的一次性迁移工作流。 |
| 14 | [`99717aa1`](https://github.com/CarterLeeAlt/sub2api/commit/99717aa1d100cb94da8945589dc86e2be50a2a47) | CI | 镜像构建前增加 fork 定向回归门禁。 |
| 15 | [`57a1f29f`](https://github.com/CarterLeeAlt/sub2api/commit/57a1f29fc6c71168d6a3a092b4a4611c4e3c58ad) | 上游同步 | 合并上游 `10a4c6e3`，按上述策略解决测试冲突。 |
| 16 | [`3e72a088`](https://github.com/CarterLeeAlt/sub2api/commit/3e72a088264568f3d744a60e45be246a82a4e9dc) | 功能 | 镜像和主界面显示一致的 Git SHA 标识。 |
| 17 | [`9f5bde85`](https://github.com/CarterLeeAlt/sub2api/commit/9f5bde85c49185826ffb4c512e9612a412c81fcf) | 上游同步 | 合并上游 `1e618dbc`，保留本地测试和动态生图逻辑，并稳定化 Windows 缓存测试。 |
| 18 | [`810eef47`](https://github.com/CarterLeeAlt/sub2api/commit/810eef477fee4303645b8d04eda21785dd919ed5) | 上游同步 | 合并上游 `5935e674`，接受 Codex 指纹默认 `session`，保留 fork 的 OAuth 模型同步、动态生图逻辑与测试。 |
| 19 | [`5306cbe37`](https://github.com/CarterLeeAlt/sub2api/commit/5306cbe374b0ffffe19d66690baf21851d0d8959) | 上游同步 | 合并上游 `fbfdcef81`，保留 OAuth 动态模型/生图、Codex 用量和界面定制，接入 Grok 4.6、逐模型定价、长上下文与 x_search；版本徽章收起状态只显示版本号。 |
| 20 | [`78fe75aa0`](https://github.com/CarterLeeAlt/sub2api/commit/78fe75aa0d5d14f6634ac9bd521ab640ad019248) | 上游同步 | 合并上游 `baeac1f3d`，推进到 `v0.1.177`，接入分组用量日汇总、Codex turn state/compaction、channel restriction 及相关测试。 |
| 21 | [`5914e9fef`](https://github.com/CarterLeeAlt/sub2api/commit/5914e9fef6e70c493c4318553102a254e72e76c1) | 构建修复 | Docker Go 构建镜像与 `backend/go.mod` 的 `1.26.6` 对齐。 |
| 22 | [`adaf4ed3d`](https://github.com/CarterLeeAlt/sub2api/commit/adaf4ed3d86b287bb3cda96a879a342bfa7a2c2b) | 测试 | 固定分组用量汇总触发器集成测试的数据库会话时区。 |
| 23 | [`23eab2e32`](https://github.com/CarterLeeAlt/sub2api/commit/23eab2e32c5a8db90feb385a0f9ce30abc426557) | 修复 | 额度恢复后清理 OpenAI 账号既有的调度阈值停调状态。 |
| 24 | [`be0367c37`](https://github.com/CarterLeeAlt/sub2api/commit/be0367c37c9ab566eb5ee5517d1508b5dc9e71b6) | 修复 | 统一 Codex 账号级停调阈值优先级，并协调编辑阈值后的既有停调状态。 |
| 25 | [`992b51eb7`](https://github.com/CarterLeeAlt/sub2api/commit/992b51eb7d601bcb2fb490f185b45722d08dfcaa) | 修复 | 为同一 Codex 请求预计算并共享起始时间，消除头、普通 JSON 和 raw 透传路径之间的 1 毫秒竞态。 |
| 26 | [`e7d474548`](https://github.com/CarterLeeAlt/sub2api/commit/e7d47454859c0ff3b618a2497f8e6246df6c1cf3) | 修复 | 以同步单调 WHAM 快照、额度代际 CAS、耐久调度元数据事件和启动重试收紧 Codex 阈值恢复。 |

## 下次同步检查清单

1. 获取 `upstream/main`，先比较当前共同祖先和上游新增提交，不直接覆盖本地分支。
2. 检查 `CUSTOM-001` 至 `CUSTOM-008` 的主要文件是否被上游修改。
3. 如果上游已经提供等价功能，比较行为和测试后将对应条目标记为 `upstreamed`；不要长期维护重复生产代码。
4. 对测试冲突按覆盖行为判断，不按来源机械选择；保留覆盖更完整且与当前实现一致的测试。
5. 不恢复 `retired` 的一次性工作流。
6. 完成合并后运行上游测试以及 `.github/workflows/custom-docker.yml` 中列出的 fork 定向回归测试。
7. 更新本文档中的已同步上游节点、冲突处理记录、提交索引和定制状态。
8. 推送后确认 GitHub Actions 成功，并核对 `latest`、`sha-xxxxxxx` 镜像标签与界面显示的 SHA 一致。

## 高冲突概率文件

后续同步上游时，优先审查以下文件：

- `backend/internal/service/upstream_models.go`
- `backend/internal/service/upstream_models_openai_oauth.go`
- `backend/internal/service/openai_images_oauth_model_selection.go`
- `backend/internal/service/openai_images_responses.go`
- `backend/internal/service/account_scheduling_threshold_eval_test.go`
- `backend/internal/service/content_moderation_runtime_cache_test.go`
- `backend/internal/service/account_usage_service.go`
- `backend/internal/service/openai_gateway_scheduling.go`
- `backend/internal/service/openai_codex_fingerprint.go`
- `backend/internal/service/ratelimit_service.go`
- `backend/internal/repository/account_repo.go`
- `backend/internal/repository/group_usage_rollup_trigger_integration_test.go`
- `frontend/src/components/account/ModelWhitelistSelector.vue`
- `frontend/src/components/account/EditAccountModal.vue`
- `.github/workflows/custom-docker.yml`
- `Dockerfile`
- `frontend/src/components/common/VersionBadge.vue`
