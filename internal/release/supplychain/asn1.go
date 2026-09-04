package supplychain

import (
	"encoding/asn1"
	"errors"
)

// asn1ObjectIdentifier builds one DER object identifier for extension
// matching.
func asn1ObjectIdentifier(parts ...int) asn1.ObjectIdentifier {
	return asn1.ObjectIdentifier(parts)
}

// unmarshalIssuerExtension decodes the Fulcio OIDC-issuer extension value.
// Fulcio writes the issuer URL as the raw extension bytes; a DER UTF8String
// wrapping is tolerated.
func unmarshalIssuerExtension(value []byte, issuer *string) error {
	if len(value) == 0 {
		return errors.New("empty OIDC issuer extension")
	}
	var text asn1.RawValue
	rest, err := asn1.Unmarshal(value, &text)
	if err == nil && len(rest) == 0 && text.Class == asn1.ClassUniversal &&
		(text.Tag == asn1.TagUTF8String || text.Tag == asn1.TagPrintableString) {
		*issuer = string(text.Bytes)
		return nil
	}
	*issuer = string(value)
	return nil
}

func base64Decode(value string) ([]byte, error) {
	return base64DecodeStd(value)
}
