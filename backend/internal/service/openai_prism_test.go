package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrismRequestBodyUsesCapturedMetadataAndMaxEffort(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Credentials: map[string]any{
		"prism_cookie": "prism_session_token=test",
		"prism_template": map[string]any{
			"metadata": map[string]any{
				"projectId": "project-1", "userId": "user-1",
				"sandbox_url": "https://sandbox.example/", "sandbox_token": "sandbox-secret",
			},
		},
	}}
	raw, model, nonce, err := prismRequestBody(account, []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"max"},"input":"hello","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, "gpt-6-astra", model)
	require.NotEmpty(t, nonce)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	metadata, ok := body["metadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "project-1", metadata["projectId"])
	require.Equal(t, map[string]any{"effort": "max"}, metadata["output_config"])
	require.NotEmpty(t, body["conversationId"])
}

func TestPrismEnvelopePayloadAndOutputText(t *testing.T) {
	payload, err := prismEnvelopePayload([]byte(`{"status":"completed","response":{"status":"success","payload":{"id":"resp_1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}}`))
	require.NoError(t, err)
	require.Equal(t, "resp_1", payload["id"])
	require.Equal(t, "hello", prismOutputText(payload))
}

func TestPrismNormalizePayloadDecodesBridgeEnvelope(t *testing.T) {
	payload := map[string]any{"model": "gpt-6-astra", "output": []any{map[string]any{
		"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": `{"protocol":"prism-codex-bridge-v1","nonce":"n1","text":"done","calls":[]}`}},
	}}}
	prismNormalizePayload(payload, "gpt-6-astra", "n1")
	require.Equal(t, "done", prismOutputText(payload))
}
