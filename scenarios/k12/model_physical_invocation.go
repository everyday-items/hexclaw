package k12

// ModelPhysicalInvocation is the immutable child receipt for one actual
// structured recognition provider request. It stores digests and frozen
// control-plane facts only; prompt text, image bytes, and raw responses are
// deliberately excluded.
type ModelPhysicalInvocation struct {
	PhysicalInvocationID  string                     `json:"physical_invocation_id"`
	ParentInvocationID    string                     `json:"parent_invocation_id"`
	AgentName             string                     `json:"agent_name"`
	JobID                 string                     `json:"job_id"`
	Stage                 string                     `json:"stage"`
	PhysicalUnit          RecognitionPhysicalUnit    `json:"physical_unit"`
	RequestDigest         string                     `json:"request_digest"`
	RouteSnapshot         GradingModelSnapshot       `json:"route_snapshot"`
	RequestPolicySnapshot ModelRequestPolicySnapshot `json:"request_policy_snapshot,omitzero"`
	Status                ModelInvocationStatus      `json:"status"`
	Attempt               int                        `json:"attempt"`
	ResultDigest          string                     `json:"result_digest,omitempty"`
	ExternalRequestID     string                     `json:"external_request_id,omitempty"`
	FailureKind           string                     `json:"failure_kind,omitempty"`
	CreatedAt             int64                      `json:"created_at"`
	UpdatedAt             int64                      `json:"updated_at"`
}
