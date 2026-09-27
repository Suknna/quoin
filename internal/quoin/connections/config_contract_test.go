package connections

import (
	"encoding/json"
	"testing"

	"github.com/Suknna/quoin/internal/contract"
)

func TestModelProviderRevisionUsesSharedConfigShape(t *testing.T) {
	input := []byte(`{"type":"model_provider","baseUrl":"https://model.example","chatModelId":"chat","contextBudgetTokens":4096,"maxOutputTokens":1024}`)
	canonical, err := validateConfig(TypeModelProvider, input)
	if err != nil {
		t.Fatal(err)
	}
	var config contract.ModelProviderConfig
	if err := json.Unmarshal(canonical, &config); err != nil {
		t.Fatal(err)
	}
	if config.ChatModelID != "chat" || config.EmbeddingModelID != "" || config.MaxOutputTokens != 1024 {
		t.Fatalf("unexpected shared revision projection: %+v", config)
	}
	if _, err := validateConfig(TypeModelProvider, []byte(`{"type":"model_provider","baseUrl":"https://model.example","chatModelId":"chat","contextBudgetTokens":"invalid"}`)); err == nil {
		t.Fatal("invalid budget type must be rejected before reaching Plinth")
	}
}
