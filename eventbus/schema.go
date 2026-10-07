package eventbus

// 事件总线三表 DDL(服务迁移 SQL 的正本在此维护,tools/migrate 下发;
// 三表为平台基础设施表,不含业务通用字段,tenantaudit 对账时按 Exempt 登记)。

// SchemaOutbox outbox 事件发件表。
const SchemaOutbox = `
CREATE TABLE IF NOT EXISTS event_outbox (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  event_id      VARCHAR(64)  NOT NULL COMMENT '事件唯一标识(去重/重投/对账键)',
  event_type    VARCHAR(128) NOT NULL COMMENT '事件类型(点号命名)',
  topic         VARCHAR(128) NOT NULL COMMENT '目标 topic(下划线命名)',
  partition_key VARCHAR(128) NOT NULL DEFAULT '' COMMENT '分区保序键=聚合ID',
  tenant_id     BIGINT       NOT NULL DEFAULT 0 COMMENT '租户ID(消费侧恢复)',
  trace_id      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '生产侧链路ID',
  payload       JSON         NOT NULL COMMENT '事件信封全文',
  status        VARCHAR(16)  NOT NULL DEFAULT 'PENDING' COMMENT 'PENDING/SENT/FAILED/DEAD',
  retry_count   INT          NOT NULL DEFAULT 0 COMMENT '投递重试次数',
  next_retry_at DATETIME(3)  NOT NULL COMMENT '下次投递时间(退避)',
  last_error    VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次投递错误',
  created_at    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_event_id (event_id),
  KEY idx_dispatch (status, next_retry_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='事件发件箱(outbox)'`

// SchemaDedup 消费去重表(与业务处理同事务写入,唯一约束幂等)。
const SchemaDedup = `
CREATE TABLE IF NOT EXISTS event_dedup (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  consumer_group VARCHAR(64) NOT NULL COMMENT '消费组(全局唯一)',
  event_id       VARCHAR(64) NOT NULL COMMENT '事件唯一标识',
  consumed_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_group_event (consumer_group, event_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='事件消费去重'`

// SchemaRetry 消费失败重投表(Relay 按 next_retry_at 退避重投)。
const SchemaRetry = `
CREATE TABLE IF NOT EXISTS event_retry (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  event_id       VARCHAR(64)  NOT NULL,
  consumer_group VARCHAR(64)  NOT NULL,
  event_type     VARCHAR(128) NOT NULL,
  topic          VARCHAR(128) NOT NULL,
  partition_key  VARCHAR(128) NOT NULL DEFAULT '',
  tenant_id      BIGINT       NOT NULL DEFAULT 0,
  trace_id       VARCHAR(64)  NOT NULL DEFAULT '',
  payload        JSON         NOT NULL COMMENT '事件信封全文(重投即重发)',
  status         VARCHAR(16)  NOT NULL DEFAULT 'RETRY' COMMENT 'RETRY(待重投)',
  retry_count    INT          NOT NULL DEFAULT 0 COMMENT '消费失败次数',
  next_retry_at  DATETIME(3)  NOT NULL,
  last_error     VARCHAR(512) NOT NULL DEFAULT '',
  created_at     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at     DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_group_event (consumer_group, event_id),
  KEY idx_dispatch (status, next_retry_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='消费失败重投'`

// SchemaDead 死信表(≤16 次重投失败的终态留底 + 告警;修复只允许补投递重放,E10)。
const SchemaDead = `
CREATE TABLE IF NOT EXISTS event_dead (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  source         VARCHAR(16)  NOT NULL COMMENT 'outbox(投递失败)/consume(消费失败)/malformed(畸形消息)',
  event_id       VARCHAR(64)  NOT NULL,
  consumer_group VARCHAR(64)  NOT NULL DEFAULT '',
  event_type     VARCHAR(128) NOT NULL DEFAULT '',
  topic          VARCHAR(128) NOT NULL DEFAULT '',
  partition_key  VARCHAR(128) NOT NULL DEFAULT '',
  tenant_id      BIGINT       NOT NULL DEFAULT 0,
  trace_id       VARCHAR(64)  NOT NULL DEFAULT '',
  payload        JSON         NOT NULL,
  retry_count    INT          NOT NULL DEFAULT 0,
  last_error     VARCHAR(512) NOT NULL DEFAULT '',
  failed_at      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  KEY idx_event (event_id),
  KEY idx_source (source, failed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='事件死信'`

// Schema 全部建表语句(测试/工具一次性建表)。
var Schema = []string{SchemaOutbox, SchemaDedup, SchemaRetry, SchemaDead}
