# AI API 中转站设计方案

> 项目代号：`llm-relay`（可自由改名）
> 技术栈：Go 1.26 + go-zero + MySQL + Redis（与 onepark 主工程保持一致）
> 生成日期：2026-09-17

## 文档索引

| 文件 | 内容 |
|---|---|
| [01-架构设计.md](01-架构设计.md) | 整体架构、模块划分、核心技术点实现方案（SSE 转发、协议转换、渠道路由、计费） |
| [02-数据库设计.md](02-数据库设计.md) | 全部表结构 DDL（渠道、令牌、用户、日志、计费） |
| [03-API设计.md](03-API设计.md) | 对内管理接口 + 对外 OpenAI 兼容接口定义、配置示例 |
| [04-参考项目分析.md](04-参考项目分析.md) | One API / New API / GPT-Load 架构分析、可借鉴清单（按优先级） |
| [deploy/schema.sql](deploy/schema.sql) | 建库建表脚本（已在本地 onepark-mysql 的 `llm_relay` 库执行，10 张表） |

## 一句话定位

统一接入多家 LLM 上游（Claude / OpenAI / DeepSeek / 火山方舟 / Ollama 等），对外暴露
OpenAI 兼容接口（`/v1/chat/completions`），提供 key 分发、渠道路由、用量计量与计费能力。
对标 One API / New API / LiteLLM，按 onepark 工程规范落地。

## 核心技术点（详见架构文档）

1. **SSE 流式透传** — 逐 chunk 转发、断连取消、首字节前可重试
2. **协议转换** — 以 OpenAI 格式为"普通话"，双向转换 Claude 格式（`/v1/messages`）
3. **渠道路由与故障转移** — 权重/优先级 + 探活 + 自动禁用
4. **计量计费** — usage 提取、预扣费、模型定价表
5. **令牌体系** — 虚拟 key（配额/模型白名单/过期时间）

## 模块规划（go-zero 风格，挂到现有 go.work）

```
app/llm-relay/               # 中转站主服务（对外 API + 管理后台接口）
app/llm-relay-admin/         # 管理后台前端（Vue3，可后置）
common/llmrelay/             # 协议转换、SSE 转发等纯逻辑库（可被复用/单测）
```

## 开发路线图

| 阶段 | 内容 | 里程碑 |
|---|---|---|
| P0 | 单渠道 SSE 透传跑通 + 令牌鉴权 + 请求日志 | `curl` 流式对话可用 |
| P1 | 协议转换（Claude ↔ OpenAI）+ 模型映射 | 客户端无感知切换上游 |
| P2 | 多渠道路由/重试/探活 + 用量计量 | 渠道故障自动切换 |
| P3 | 用户体系 + 配额扣费 + 管理后台 | 可对外提供服务 |

## 参考项目

- [One API](https://github.com/songquanpeng/one-api)（Go，整体结构最接近）
- [New API](https://github.com/QuantumNous/new-api)（One API 的活跃分支，功能更全）
- [LiteLLM](https://github.com/BerriAI/litellm)（Python，协议转换实现最全，当"转换规格说明书"读）
