package contract

// ModelProviderConfig is the non-secret revision projection shared by Quoin's
// connection validation and Plinth's model-provider execution.
type ModelProviderConfig struct {
	Type                string `json:"type"`
	BaseURL             string `json:"baseUrl"`
	ChatModelID         string `json:"chatModelId"`
	EmbeddingModelID    string `json:"embeddingModelId"`
	ContextBudgetTokens int    `json:"contextBudgetTokens"`
	MaxOutputTokens     int    `json:"maxOutputTokens"`
}
