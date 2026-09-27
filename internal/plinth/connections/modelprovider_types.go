package connections

import "github.com/Suknna/quoin/internal/contract"

// ModelProviderConfig is the non-secret model provider revision projection.
type ModelProviderConfig = contract.ModelProviderConfig

// ModelProviderSecret is the decrypted API key carrier.
type ModelProviderSecret struct {
	APIKey string `json:"apiKey"`
}
