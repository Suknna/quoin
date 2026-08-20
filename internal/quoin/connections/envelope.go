package connections

// Envelope codec bridging to the shared AEAD implementation in
// internal/quoin/secrets, kept behind tiny aliases so the connections domain
// does not import crypto details everywhere.

import (
	"github.com/Suknna/quoin/internal/quoin/secrets"
)

const envelopeVersion = secrets.EnvelopeVersion

type envelopeWire = secrets.Envelope

func sealEnvelope(rootKey []byte, connectionID, generationSeq int64, connectionType string, bindingRevision int, typed *typedSecretJSON) (*envelopeWire, error) {
	return secrets.Seal(rootKey, connectionID, generationSeq, connectionType, bindingRevision, &secrets.TypedSecret{
		Type:          typed.Type,
		Thanos:        thanosOf(typed.Thanos),
		Kubernetes:    kubernetesOf(typed.Kubernetes),
		ModelProvider: modelProviderOf(typed.ModelProvider),
	})
}

func openEnvelope(rootKey []byte, connectionID, generationSeq int64, connectionType string, bindingRevision int, nonce, ciphertext []byte) (*typedSecretJSON, error) {
	secret, err := secrets.Open(rootKey, connectionID, generationSeq, connectionType, bindingRevision, &secrets.Envelope{Nonce: nonce, Ciphertext: ciphertext})
	if err != nil {
		return nil, err
	}
	return &typedSecretJSON{
		Type:          secret.Type,
		Thanos:        thanosFrom(secret.Thanos),
		Kubernetes:    kubernetesFrom(secret.Kubernetes),
		ModelProvider: modelProviderFrom(secret.ModelProvider),
	}, nil
}

func thanosOf(source *thanosSecretJSON) *secrets.ThanosSecret {
	if source == nil {
		return nil
	}
	return &secrets.ThanosSecret{Username: source.Username, Password: source.Password}
}

func thanosFrom(source *secrets.ThanosSecret) *thanosSecretJSON {
	if source == nil {
		return nil
	}
	return &thanosSecretJSON{Username: source.Username, Password: source.Password}
}

func kubernetesOf(source *kubernetesSecretJSON) *secrets.KubernetesSecret {
	if source == nil {
		return nil
	}
	return &secrets.KubernetesSecret{Kubeconfig: source.Kubeconfig}
}

func kubernetesFrom(source *secrets.KubernetesSecret) *kubernetesSecretJSON {
	if source == nil {
		return nil
	}
	return &kubernetesSecretJSON{Kubeconfig: source.Kubeconfig}
}

func modelProviderOf(source *modelProviderSecretJSON) *secrets.ModelProviderSecret {
	if source == nil {
		return nil
	}
	return &secrets.ModelProviderSecret{APIKey: source.APIKey}
}

func modelProviderFrom(source *secrets.ModelProviderSecret) *modelProviderSecretJSON {
	if source == nil {
		return nil
	}
	return &modelProviderSecretJSON{APIKey: source.APIKey}
}
