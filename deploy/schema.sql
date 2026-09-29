-- llm-relay 数据库 Schema
-- 对应设计文档：02-数据库设计.md（2026-09-29 版）
-- MySQL 8.x，utf8mb4。金额/配额统一用整数（1 quota = 0.0001 元）。

CREATE DATABASE IF NOT EXISTS `llm_relay`
  CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE `llm_relay`;

-- ---------------------------------------------------------------
-- 1. lr_channel — 上游渠道
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `lr_channel` (
  `id`             BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `name`           VARCHAR(64)  NOT NULL COMMENT '渠道名称',
  `type`           VARCHAR(32)  NOT NULL COMMENT '适配器类型: openai / anthropic / ark / ollama / custom',
  `base_url`       VARCHAR(255) NOT NULL COMMENT '上游地址',
  `models`         JSON         NOT NULL COMMENT '该渠道支持的模型列表 ["gpt-4o","claude-sonnet-5"]',
  `priority`       INT          NOT NULL DEFAULT 0 COMMENT '越大越优先',
  `weight`         INT          NOT NULL DEFAULT 1 COMMENT '同优先级内加权随机权重',
  `status`         TINYINT      NOT NULL DEFAULT 1 COMMENT '1启用 0手动禁用 -1自动禁用(探活失败)',
  `proxy_url`      VARCHAR(255) DEFAULT '' COMMENT '出站代理(可选)',
  `test_model`     VARCHAR(128) DEFAULT '' COMMENT '探活用模型',
  `last_test_at`   DATETIME     NULL,
  `last_test_ok`   TINYINT      NULL,
  `fail_count`     INT          NOT NULL DEFAULT 0 COMMENT '连续失败次数(自动禁用判断)',
  `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  KEY `idx_status_priority` (`status`, `priority`)
) COMMENT '上游渠道';

-- 上游真实 key 不放在本表，拆到 lr_channel_key（1:N）。
-- P0 阶段每渠道只配 1 个 key，表结构按多 key 设计，P2 做 key 池调度时无需迁移。

-- ---------------------------------------------------------------
-- 1.1 lr_channel_key — 渠道上游 key
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `lr_channel_key` (
  `id`             BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `channel_id`     BIGINT UNSIGNED NOT NULL,
  `api_key_cipher` VARBINARY(512) NOT NULL COMMENT 'AES-GCM 加密后的真实 key，主密钥来自 RELAY_MASTER_KEY',
  `weight`         INT          NOT NULL DEFAULT 1 COMMENT 'key 级权重(P2 key 池用)',
  `status`         TINYINT      NOT NULL DEFAULT 1 COMMENT '1启用 0禁用 -1冷却中(429/超限)',
  `fail_count`     INT          NOT NULL DEFAULT 0 COMMENT '该 key 连续失败次数',
  `cooldown_until` DATETIME     NULL COMMENT '冷却截止(模型级冷却 P2 落地)',
  `created_at`     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  KEY `idx_channel` (`channel_id`, `status`)
) COMMENT '渠道上游key';

-- ---------------------------------------------------------------
-- 2. lr_model_mapping — 模型映射（客户端可见名 → 渠道真实名）
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `lr_model_mapping` (
  `id`            BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `public_name`   VARCHAR(128) NOT NULL COMMENT '对外模型名，客户端请求里填的',
  `channel_id`    BIGINT UNSIGNED NOT NULL COMMENT '走哪个渠道',
  `real_name`     VARCHAR(128) NOT NULL COMMENT '请求上游时替换成的真实模型名',
  `enabled`       TINYINT NOT NULL DEFAULT 1,
  KEY `idx_public` (`public_name`),
  KEY `idx_channel` (`channel_id`)
) COMMENT '模型映射';

-- ---------------------------------------------------------------
-- 2.1 lr_ability — 路由索引表（借鉴 One API ability 模式）
-- 渠道/模型映射保存时全量重建该渠道的行（先删后插）。
-- 运行时整表加载进内存快照，请求路径不查库。
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `lr_ability` (
  `group_id`    BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '用户组，0=不限组',
  `public_name` VARCHAR(128) NOT NULL COMMENT '对外模型名',
  `channel_id`  BIGINT UNSIGNED NOT NULL,
  `enabled`     TINYINT NOT NULL DEFAULT 1,
  `priority`    INT NOT NULL DEFAULT 0 COMMENT '冗余自 channel，越大越优先',
  `weight`      INT NOT NULL DEFAULT 1 COMMENT '冗余自 channel',
  PRIMARY KEY (`group_id`, `public_name`, `channel_id`),
  KEY `idx_channel` (`channel_id`)
) COMMENT '路由索引(渠道保存时重建)';

-- ---------------------------------------------------------------
-- 3. lr_model_pricing — 定价表
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `lr_model_pricing` (
  `id`              BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `model_name`      VARCHAR(128) NOT NULL COMMENT '模型名(对齐 mapping.real_name)',
  `input_per_m`     BIGINT NOT NULL COMMENT '每 1M 输入 token 价格(quota)',
  `output_per_m`    BIGINT NOT NULL COMMENT '每 1M 输出 token 价格(quota)',
  `cached_input_per_m` BIGINT NOT NULL DEFAULT 0 COMMENT '缓存命中输入价(0=不支持)',
  `updated_at`      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_model` (`model_name`)
) COMMENT '模型定价';

-- ---------------------------------------------------------------
-- 4. lr_user — 用户
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `lr_user` (
  `id`           BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `username`     VARCHAR(64) NOT NULL,
  `password_hash` VARCHAR(255) NOT NULL COMMENT 'bcrypt',
  `role`         TINYINT NOT NULL DEFAULT 2 COMMENT '1管理员 2普通用户',
  `group_id`     BIGINT UNSIGNED NULL COMMENT '用户组(倍率)',
  `quota`        BIGINT NOT NULL DEFAULT 0 COMMENT '可用配额(预扣冻结只存在于Redis)',
  `used_quota`   BIGINT NOT NULL DEFAULT 0 COMMENT '累计消耗',
  `status`       TINYINT NOT NULL DEFAULT 1,
  `created_at`   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_username` (`username`)
) COMMENT '用户';

-- ---------------------------------------------------------------
-- 5. lr_user_group — 用户组（计费倍率）
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `lr_user_group` (
  `id`      BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `name`    VARCHAR(64) NOT NULL,
  `ratio`   DECIMAL(6,3) NOT NULL DEFAULT 1.000 COMMENT '计费倍率，1.0=原价',
  `enabled` TINYINT NOT NULL DEFAULT 1
) COMMENT '用户组';

-- ---------------------------------------------------------------
-- 6. lr_token — 虚拟令牌（发给下游的 key）
-- 只存 hash 不存明文：创建时生成 sk-relay- + 32 位随机串，
-- 明文返回一次，落库存 sha256。
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `lr_token` (
  `id`            BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `user_id`       BIGINT UNSIGNED NOT NULL,
  `name`          VARCHAR(64) NOT NULL DEFAULT '' COMMENT '备注名',
  `key_hash`      CHAR(64) NOT NULL COMMENT 'sha256(完整key)，明文只在创建时返回一次',
  `key_prefix`    VARCHAR(16) NOT NULL COMMENT 'sk-relay-abc12 形式的前8位，用于展示',
  `quota`         BIGINT NOT NULL DEFAULT -1 COMMENT '令牌独立配额，-1=不限(受用户配额约束)',
  `used_quota`    BIGINT NOT NULL DEFAULT 0,
  `models`        JSON NULL COMMENT '模型白名单，NULL=不限',
  `expired_at`    DATETIME NULL COMMENT 'NULL=永不过期',
  `ip_limit`      VARCHAR(255) DEFAULT '' COMMENT '允许IP列表，逗号分隔，空=不限',
  `rpm_limit`     INT NOT NULL DEFAULT 0 COMMENT '每分钟请求数上限，0=不限',
  `status`        TINYINT NOT NULL DEFAULT 1,
  `created_at`    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_key_hash` (`key_hash`),
  KEY `idx_user` (`user_id`)
) COMMENT '虚拟令牌';

-- ---------------------------------------------------------------
-- 7. lr_relay_log — 请求日志
-- 日志量大，写入走内存 channel 异步批量 insert；按月分表或定期归档。
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `lr_relay_log` (
  `id`             BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  `trace_id`       CHAR(32) NOT NULL COMMENT '贯穿全链路',
  `user_id`        BIGINT UNSIGNED NOT NULL,
  `token_id`       BIGINT UNSIGNED NOT NULL,
  `channel_id`     BIGINT UNSIGNED NULL,
  `model_request`  VARCHAR(128) NOT NULL COMMENT '客户端请求的模型名',
  `model_real`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '实际路由到的模型',
  `prompt_tokens`  INT NOT NULL DEFAULT 0,
  `completion_tokens` INT NOT NULL DEFAULT 0,
  `usage_estimated` TINYINT NOT NULL DEFAULT 0 COMMENT '1=usage为估算(上游未返回)',
  `quota_cost`     BIGINT NOT NULL DEFAULT 0 COMMENT '本次实际扣费',
  `duration_ms`    INT NOT NULL DEFAULT 0,
  `first_byte_ms`  INT NOT NULL DEFAULT 0 COMMENT '首字耗时(体验指标)',
  `stream`         TINYINT NOT NULL DEFAULT 0,
  `status`         TINYINT NOT NULL COMMENT '1成功 2上游失败 3限流拒绝 4余额不足 5鉴权失败',
  `http_status`    INT NOT NULL DEFAULT 0,
  `error_msg`      VARCHAR(1024) DEFAULT '',
  `client_ip`      VARCHAR(64) NOT NULL DEFAULT '',
  `created_at`     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  KEY `idx_user_time` (`user_id`, `created_at`),
  KEY `idx_channel_time` (`channel_id`, `created_at`),
  KEY `idx_created` (`created_at`)
) COMMENT '中转请求日志';

-- ---------------------------------------------------------------
-- 7.1 lr_usage_hourly — 用量预聚合（借鉴 New API usedata）
-- 结算时同批次累加，看板/汇总查这张表，不动 lr_relay_log。
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `lr_usage_hourly` (
  `hour`              DATETIME NOT NULL COMMENT '整点，如 2026-09-28 14:00:00',
  `user_id`           BIGINT UNSIGNED NOT NULL,
  `channel_id`        BIGINT UNSIGNED NOT NULL DEFAULT 0,
  `model_request`     VARCHAR(128) NOT NULL,
  `request_count`     INT NOT NULL DEFAULT 0,
  `fail_count`        INT NOT NULL DEFAULT 0,
  `prompt_tokens`     BIGINT NOT NULL DEFAULT 0,
  `completion_tokens` BIGINT NOT NULL DEFAULT 0,
  `quota_cost`        BIGINT NOT NULL DEFAULT 0,
  `duration_ms_total` BIGINT NOT NULL DEFAULT 0 COMMENT '算平均值用',
  `first_byte_ms_total` BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (`hour`, `user_id`, `channel_id`, `model_request`)
) COMMENT '小时级用量聚合';
