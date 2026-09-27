package contract

// PromQLArtifactRef identifies the sealed full body of a spilled query result.
type PromQLArtifactRef struct {
	ID         string `json:"id"`
	MediaType  string `json:"mediaType"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"sizeBytes"`
	TotalLines int64  `json:"totalLines"`
}

// PromQLQueryResult is the frozen thanos_query_result_v1 payload. Field order
// matches the producer's original JSON encoding, including omitted fields.
type PromQLQueryResult struct {
	Success     bool               `json:"success"`
	StartedAt   string             `json:"startedAt"`
	FinishedAt  string             `json:"finishedAt"`
	ErrorCode   string             `json:"errorCode,omitempty"`
	ErrorDetail string             `json:"errorDetail,omitempty"`
	Status      string             `json:"status,omitempty"`
	ResultType  string             `json:"resultType,omitempty"`
	SampleCount int                `json:"sampleCount,omitempty"`
	Truncated   bool               `json:"truncated"`
	TotalBytes  int64              `json:"totalBytes"`
	TotalLines  int64              `json:"totalLines"`
	Output      string             `json:"output"`
	Artifact    *PromQLArtifactRef `json:"artifact,omitempty"`
}
