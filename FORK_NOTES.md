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
| 当前已同步上游节点 | [`5935e674`](https://github.com/Wei-Shaw/sub2api/commit/5935e674a84341c3536e27e6a968384f67d9062b) |
| 最近一次上游合并提交 | [`810eef47`](https://github.com/CarterLeeAlt/sub2api/commit/810eef477fee4303645b8d04eda21785dd919ed5) |

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

主要文件：

- `.github/workflows/custom-docker.yml`
- `Dockerfile`

相关提交：[`40a7694b`](https://github.com/CarterLeeAlt/sub2api/commit/40a7694b2ce8cb9aadf18b340d1a875f253de6f8)、[`99717aa1`](https://github.com/CarterLeeAlt/sub2api/commit/99717aa1d100cb94da8945589dc86e2be50a2a47)。

### CUSTOM-004：界面显示镜像构建标识（`active`）

版本徽标在上游版本号后显示与镜像 `sha-xxxxxxx` 标签一致的 7 位提交标识，例如 `v0.1.173 · sha-3e72a08`。这用于确认正在运行的容器是否来自预期的 `latest` 构建，不负责自动检查或拉取镜像更新。

主要文件：

- `Dockerfile`
- `frontend/src/components/common/VersionBadge.vue`
- `frontend/src/utils/buildIdentity.ts`
- `frontend/src/utils/__tests__/buildIdentity.spec.ts`
- `frontend/src/vite-env.d.ts`

相关提交：[`3e72a088`](https://github.com/CarterLeeAlt/sub2api/commit/3e72a088264568f3d744a60e45be246a82a4e9dc)。

### CUSTOM-005：内容审核缓存测试稳定化（`active`，仅测试）

`TestContentModerationRuntimeSnapshotRefreshFailureKeepsStaleConfig` 不再使用 `1ns` TTL 假设两次相邻的 Windows 时钟读取必然不同。测试改为显式把运行时快照设为过期，保持“异步刷新失败时继续使用旧配置”的原始验证目标，同时消除与 Windows 时钟粒度相关的不稳定失败。

主要文件：

- `backend/internal/service/content_moderation_runtime_cache_test.go`

相关提交：[`9f5bde85`](https://github.com/CarterLeeAlt/sub2api/commit/9f5bde85c49185826ffb4c512e9612a412c81fcf)。

## 已被上游吸收

### OpenAI 调度阈值百分比语义（`upstreamed`）

fork 最初修正了 Codex 调度用量的单位，确保阈值比较使用百分比而不是小数比例。同步到上游节点 `10a4c6e3` 时，上游已通过 PR `#5468` 提供等价生产实现，因此不再把生产代码视为 fork 独有功能。

本地保留了覆盖 OpenAI 和 Anthropic 百分比语义的表驱动测试，作为后续上游合并的回归保护。

相关提交：[`d39b99a3`](https://github.com/CarterLeeAlt/sub2api/commit/d39b99a3bd9339882a0c0c6b9eccea7e45aa52dd)、[`2e61f7d4`](https://github.com/CarterLeeAlt/sub2api/commit/2e61f7d4a767e8ad833103c26e853da43b2107c4)。

## 已退役机制

### 一次性生图主模型迁移工作流（`retired`）

提交 `c3031d0e` 曾加入一次性 GitHub Actions 工作流，用于把动态生图主模型改动应用到源码；随后由 `abd725ec` 落地实际源码变更，并在 `5791cb14` 删除该工作流。

后续合并不得恢复已删除的一次性工作流。动态生图实现本身仍属于 `CUSTOM-002`，继续保留。

相关提交：[`c3031d0e`](https://github.com/CarterLeeAlt/sub2api/commit/c3031d0ef726af217307639afd270df71097ab4d)、[`abd725ec`](https://github.com/CarterLeeAlt/sub2api/commit/abd725ece1170f3acf831be8a8d7af3c0bc55949)、[`5791cb14`](https://github.com/CarterLeeAlt/sub2api/commit/5791cb1449ace7ce136e1fd3192fb9d8294b5585)。

## 已知上游合并处理

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

## 下次同步检查清单

1. 获取 `upstream/main`，先比较当前共同祖先和上游新增提交，不直接覆盖本地分支。
2. 检查 `CUSTOM-001` 至 `CUSTOM-005` 的主要文件是否被上游修改。
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
- `frontend/src/components/account/ModelWhitelistSelector.vue`
- `.github/workflows/custom-docker.yml`
- `Dockerfile`
- `frontend/src/components/common/VersionBadge.vue`
