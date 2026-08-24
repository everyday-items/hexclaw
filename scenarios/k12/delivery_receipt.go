package k12

import "github.com/hexagon-codes/hexclaw/messagecontent"

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
	DeliveryID         string                  `json:"delivery_id"`
	BatchID            string                  `json:"batch_id,omitempty"`
	BatchOrdinal       int                     `json:"batch_ordinal,omitempty"`
	PartKind           messagecontent.PartKind `json:"part_kind"`
	PartMIME           string                  `json:"part_mime,omitempty"`
	PartOrdinal        int                     `json:"part_ordinal"`
	PartDigest         string                  `json:"part_digest"`
	PreparedResourceID string                  `json:"-"`
	AgentName          string                  `json:"agent_name"`
	ObjectKind         string                  `json:"object_kind"`
	ObjectID           string                  `json:"object_id"`
	BindingID          string                  `json:"binding_id"`
	Target             DeliveryTarget          `json:"target"`
	Status             DeliveryReceiptStatus   `json:"status"`
	DedupeKey          string                  `json:"dedupe_key"`
	PayloadDigest      string                  `json:"payload_digest"`
	PayloadJSON        string                  `json:"payload_json"`
	RenderJSON         string                  `json:"render_manifest_json"`
	ExternalMessageID  string                  `json:"external_message_id,omitempty"`
	Attempt            int                     `json:"attempt"`
	LastError          string                  `json:"last_error,omitempty"`
	CreatedAt          int64                   `json:"created_at"`
	UpdatedAt          int64                   `json:"updated_at"`
}

type DeliveryBatchStatus string

const (
	DeliveryBatchPending        DeliveryBatchStatus = "pending"
	DeliveryBatchSending        DeliveryBatchStatus = "sending"
	DeliveryBatchDelivered      DeliveryBatchStatus = "delivered"
	DeliveryBatchFailed         DeliveryBatchStatus = "failed"
	DeliveryBatchPartialFailed  DeliveryBatchStatus = "partial_failed"
	DeliveryBatchOutcomeUnknown DeliveryBatchStatus = "outcome_unknown"
)

// DeliveryBatch freezes one logical send command and its complete binding
// snapshot. Receipts are ordered by BatchOrdinal and are the only mutable
// provider-facing state; the batch status is a projection of those children.
type DeliveryBatch struct {
	BatchID       string              `json:"batch_id"`
	AgentName     string              `json:"agent_name"`
	ObjectKind    string              `json:"object_kind"`
	ObjectID      string              `json:"object_id"`
	DedupeKey     string              `json:"dedupe_key"`
	ContentDigest string              `json:"content_digest"`
	Status        DeliveryBatchStatus `json:"status"`
	Receipts      []DeliveryReceipt   `json:"receipts"`
	CreatedAt     int64               `json:"created_at"`
	UpdatedAt     int64               `json:"updated_at"`
}

func DeliveryBatchStatusOf(receipts []DeliveryReceipt) DeliveryBatchStatus {
	if len(receipts) == 0 {
		return DeliveryBatchPending
	}
	var pending, sending, delivered, failed, unknown int
	for _, receipt := range receipts {
		switch receipt.Status {
		case DeliveryPending:
			pending++
		case DeliverySending:
			sending++
		case DeliveryDelivered:
			delivered++
		case DeliveryFailed:
			failed++
		case DeliveryOutcomeUnknown:
			unknown++
		}
	}
	switch {
	case pending == len(receipts):
		return DeliveryBatchPending
	case unknown > 0:
		return DeliveryBatchOutcomeUnknown
	case pending+sending > 0:
		return DeliveryBatchSending
	case delivered == len(receipts):
		return DeliveryBatchDelivered
	case failed == len(receipts):
		return DeliveryBatchFailed
	case delivered > 0 && failed > 0:
		return DeliveryBatchPartialFailed
	default:
		return DeliveryBatchSending
	}
}
