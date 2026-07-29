package k12

// ModelInvocationStatus is the durable external-call state. Exactly-once is
// promised only for local domain effects; sent without a conclusive response is
// outcome_unknown and cannot be blindly replayed.
type ModelInvocationStatus string

const (
	ModelInvocationPrepared       ModelInvocationStatus = "prepared"
	ModelInvocationSent           ModelInvocationStatus = "sent"
	ModelInvocationSucceeded      ModelInvocationStatus = "succeeded"
	ModelInvocationFailed         ModelInvocationStatus = "failed"
	ModelInvocationOutcomeUnknown ModelInvocationStatus = "outcome_unknown"
	ModelInvocationReconciled     ModelInvocationStatus = "reconciled"
)

type ModelInvocation struct {
	InvocationID           string                     `json:"invocation_id"`
	AgentName              string                     `json:"agent_name"`
	JobID                  string                     `json:"job_id"`
	Stage                  string                     `json:"stage"`
	RequestDigest          string                     `json:"request_digest"`
	RouteSnapshot          GradingModelSnapshot       `json:"route_snapshot"`
	RequestPolicySnapshot  ModelRequestPolicySnapshot `json:"request_policy_snapshot,omitzero"`
	ProviderIdempotencyKey string                     `json:"provider_idempotency_key,omitempty"`
	Status                 ModelInvocationStatus      `json:"status"`
	Attempt                int                        `json:"attempt"`
	ResultDigest           string                     `json:"result_digest,omitempty"`
	ExternalRequestID      string                     `json:"external_request_id,omitempty"`
	FailureKind            string                     `json:"failure_kind,omitempty"`
	CreatedAt              int64                      `json:"created_at"`
	UpdatedAt              int64                      `json:"updated_at"`
}
