# Security Policy

[English](#english) | [中文](#中文)

## English

### Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues, pull requests, or discussions.**

For this fork, consult the repository homepage for maintainer contact information:

**[CarterLeeAlt/sub2api](https://github.com/CarterLeeAlt/sub2api)**

Agree on a private reporting channel before sharing sensitive details. The repository homepage is a contact entry point, not a private vulnerability submission form.

Issues independently reproduced in the upstream project can also be reported under the [upstream security policy](https://github.com/Wei-Shaw/sub2api/blob/main/.github/SECURITY.md).

A useful report includes:

- The affected component and version (release tag or commit)
- Impact: what an attacker can do and which privileges they need
- Minimal reproduction steps or a proof of concept
- Any non-default configuration required to trigger the issue
- A suggested fix, if you have one

### Supported Versions

This fork publishes its current builds from `main`; release-based installation requires a Release published in this repository.

| Version | Supported |
| ------- | --------- |
| Current fork `main` | ✅ |
| Older fork builds | ❌ |

Please confirm that the issue reproduces on this fork's current `main` before reporting.

### Scope

In scope: the Sub2API backend, frontend, official Docker images, and deployment files in this repository.

Usually out of scope:

- Issues that require already-compromised admin credentials or server access
- Risks arising from explicitly weakening documented security settings (for example, disabling `security.url_allowlist`)
- Vulnerabilities in upstream AI providers or third-party dependencies with no Sub2API-specific impact (please report those upstream)
- Missing best-practice headers or scanner output without a demonstrated impact

### Disclosure Process

1. We acknowledge the report and assess its validity and severity.
2. We coordinate the fix through the agreed private channel and may ask you to verify it.
3. We publish the fix and coordinate disclosure, crediting you unless you prefer to stay anonymous.

Please keep details private until disclosure is agreed. This is a volunteer-maintained project, so response times are best effort.

## 中文

### 报告漏洞

**请不要通过公开的 Issue、Pull Request 或 Discussion 报告安全漏洞。**

本 Fork 的维护者联系信息以仓库主页为准：

**[CarterLeeAlt/sub2api](https://github.com/CarterLeeAlt/sub2api)**

请先与维护者确认私密报告渠道，再提供敏感细节。仓库主页是联系入口，不是私密漏洞提交表单。

如果问题也能在上游项目独立复现，可以按[上游安全策略](https://github.com/Wei-Shaw/sub2api/blob/main/.github/SECURITY.md)向上游报告。

报告中建议包含：

- 受影响的组件与版本（Release tag 或 commit）
- 影响：攻击者能做什么、需要什么权限
- 最小复现步骤或 PoC
- 触发问题所需的非默认配置
- 修复建议（如有）

### 支持的版本

本 Fork 从 `main` 发布当前构建；基于 Release 的安装需要本仓库发布相应 Release。

| 版本 | 是否支持 |
| ---- | -------- |
| Fork 当前 `main` | ✅ |
| Fork 较早构建 | ❌ |

报告前请先确认问题在本 Fork 当前 `main` 上仍可复现。

### 范围

范围内：本仓库中的 Sub2API 后端、前端、官方 Docker 镜像与部署文件。

通常不在范围内：

- 需要事先已掌握管理员凭据或服务器权限才能利用的问题
- 因主动关闭文档中的安全配置（如关闭 `security.url_allowlist`）而产生的风险
- 上游 AI 服务商或第三方依赖自身的漏洞，且对 Sub2API 无特定影响（请向上游报告）
- 仅缺少最佳实践响应头或扫描器输出，但未证明实际影响

### 披露流程

1. 我们确认收到报告，并评估其有效性与严重程度。
2. 我们通过约定的私密渠道协调修复，可能请你协助验证。
3. 我们发布修复并协商披露安排，致谢报告者（如你希望匿名则不署名）。

请在约定披露前保持细节私密。本项目由志愿者维护，响应时间尽力而为。
