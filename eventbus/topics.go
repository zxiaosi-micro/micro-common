package eventbus

// Topic 常量(下划线命名,沿袭历史事件契约,E2;由 tools/mqinit 预创建)。
// 事件类型(event_type)用点号命名,topic 与 type 是两个维度。
const (
	// —— 订单/交易域(S5-01 预建清单)——
	TopicOrderCreated        = "order_created"
	TopicOrderPaid           = "order_paid"
	TopicOrderCancelled      = "order_cancelled"
	TopicOrderPayTimeout     = "order_pay_timeout"
	TopicStockIn             = "stock_in"
	TopicStockOut            = "stock_out"
	TopicStockLow            = "stock_low"
	TopicPaymentRefunded     = "payment_refunded"
	TopicOrderReturnApproved = "order_return_approved"
	TopicWarrantyStarted     = "warranty_started"

	// —— 基础三件套 ——
	TopicNotificationRequest = "notification_request"
	TopicAuditEvent          = "audit_event"
	TopicExportRequested     = "export_requested"

	// —— IoT / 资产域 ——
	TopicIotTelemetryRaw = "iot_telemetry_raw"
	TopicAlertCandidate  = "alert_candidate"
	TopicDeviceActivated = "device_activated"
	TopicShipmentSigned  = "shipment_signed"
	TopicCmdAck          = "cmd_ack"
	TopicCmdFailed       = "cmd_failed"
	TopicOtaPaused       = "ota_paused"
	TopicDeviceAnomaly   = "device_anomaly"
	TopicStationCreated  = "station_created"
)
