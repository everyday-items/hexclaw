package adapter

import "testing"

func TestApplyRequestSamplingOverrides_StripsTrustedAgentKeysAndCapsTokens(t *testing.T) {
	metadata := map[string]string{
		"agent_temperature": "3",
		"agent_max_tokens":  "999999999",
	}
	if err := ApplyRequestSamplingOverrides(metadata, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := metadata["agent_temperature"]; ok {
		t.Fatal("untrusted agent_temperature survived ingress sanitization")
	}
	if _, ok := metadata["agent_max_tokens"]; ok {
		t.Fatal("untrusted agent_max_tokens survived ingress sanitization")
	}

	excessive := 1_000_001
	if err := ApplyRequestSamplingOverrides(metadata, nil, &excessive); err == nil {
		t.Fatal("excessive max_tokens must fail closed")
	}
}
