package subjects_test

import (
	"encoding/base64"
	"encoding/json"
)

func jsonUnmarshal(data []byte, target any) error { return json.Unmarshal(data, target) }

func jsonMarshal(value any) ([]byte, error) { return json.Marshal(value) }

func base64DecodeString(value string) ([]byte, error) { return base64.StdEncoding.DecodeString(value) }

func base64EncodeString(value []byte) string { return base64.StdEncoding.EncodeToString(value) }
