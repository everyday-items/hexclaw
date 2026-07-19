package k12

// DeliveryReceiptStatus is the durable user-visible truth for a proactive
// direct-message delivery. Transport acceptance is intentionally represented
// by sending plus ExternalMessageID; it is never a delivered terminal.
type DeliveryReceiptStatus string

const (
	DeliveryPending        DeliveryReceiptStatus = "pending"
	DeliverySending        DeliveryReceiptStatus = "sending"
	DeliveryDelivered      DeliveryReceiptStatus = "delivered"
	DeliveryFailed         DeliveryReceiptStatus = "failed"
	DeliveryOutcomeUnknown DeliveryReceiptStatus = "outcome_unknown"
)

type DeliveryTarget struct {
	Platform   string `json:"platform"`
	InstanceID string `json:"instance_id,omitempty"`
	ChatID     string `json:"chat_id"`
	Label      string `json:"label,omitempty"`
}

type DeliveryReceipt struct {
	DeliveryID        string                `json:"delivery_id"`
	AgentName         string                `json:"agent_name"`
	ObjectKind        string                `json:"object_kind"`
	ObjectID          string                `json:"object_id"`
	BindingID         string                `json:"binding_id"`
	Target            DeliveryTarget        `json:"target"`
	Status            DeliveryReceiptStatus `json:"status"`
	DedupeKey         string                `json:"dedupe_key"`
	PayloadDigest     string                `json:"payload_digest"`
	PayloadJSON       string                `json:"payload_json"`
	RenderJSON        string                `json:"render_manifest_json"`
	ExternalMessageID string                `json:"external_message_id,omitempty"`
	Attempt           int                   `json:"attempt"`
	LastError         string                `json:"last_error,omitempty"`
	CreatedAt         int64                 `json:"created_at"`
	UpdatedAt         int64                 `json:"updated_at"`
}
