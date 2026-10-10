package eventbus

// 事件类型（event_type）正本：点号命名 <域>.<动作>（S5-01 铁律，02 §6.6）。
//
// 三个维度分工：
//   - topic：Kafka 物理主题，下划线命名（见 topics.go），tools/mqinit 预创建；
//   - event_type：信封 Envelope.EventType，点号命名，消费方按类型分流；
//   - key：PushWithKey 聚合 ID（order_no/sn/saga_id），同聚合同分区保序。
//
// 消费组 Group 名全局唯一（E2）：约定 <svc>-<用途>（如 order-saga / finance-refund），
// 新消费方落库前先在 micro-docs 的 topic-消费组对照表登记。
const (
	// —— 订单域 ——
	TypeOrderCreated    = "order.created"
	TypeOrderPaid       = "order.paid"
	TypeOrderCancelled  = "order.cancelled"
	TypeOrderPayTimeout = "order.pay_timeout"

	// —— 库存域（inventory 发出，order/ops/device 消费）——
	TypeStockIn  = "stock.in"
	TypeStockOut = "stock.out"
	TypeStockLow = "stock.low"

	// —— 财务域 ——
	TypePaymentRefunded     = "payment.refunded"
	TypeOrderReturnApproved = "order.return_approved"

	// —— 合同/质保域 ——
	TypeWarrantyStarted = "warranty.started"

	// —— 基础三件套 ——
	TypeNotificationRequest = "notification.request"
	TypeAuditEvent          = "audit.event"
	TypeExportRequested     = "export.requested"

	// —— IoT / 资产域（S6 起使用）——
	TypeDeviceActivated = "device.activated"
	TypeShipmentSigned  = "shipment.signed"
	TypeCmdAck          = "cmd.ack"
	TypeCmdFailed       = "cmd.failed"
	TypeOtaPaused       = "ota.paused"
	TypeDeviceAnomaly   = "device.anomaly"

	// —— 场站域（station 发出，S6-02；ops 消费建巡检计划）——
	TypeStationCreated = "station.created"

	// —— 运维域（ops 发出，S7-01；告警闭环/聚合通知）——
	TypeAlertCreated = "alert.created"
)
