package connections

// ModelProviderConfig is the non-secret model provider revision projection.
type ModelProviderConfig struct {
	Type                string `json:"type"`
	BaseURL             string `json:"baseUrl"`
	ChatModelID         string `json:"chatModelId"`
	EmbeddingModelID    string `json:"embeddingModelId"`
	ContextBudgetTokens int    `json:"contextBudgetTokens"`
	MaxOutputTokens     int    `json:"maxOutputTokens"`
}

// ModelProviderSecret is the decrypted API key carrier.
type ModelProviderSecret struct {
	APIKey string `json:"apiKey"`
}
