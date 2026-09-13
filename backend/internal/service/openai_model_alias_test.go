package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeKnownOpenAICodexModelGPT6Astra(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "openai/gpt-6-astra", "OPENAI/GPT-6_ASTRA", "gpt-6", "openai/gpt-6"} {
		require.Equal(t, "gpt-6-astra", normalizeKnownOpenAICodexModel(model))
	}
}

func TestNormalizeKnownOpenAICodexModel_BareGPT56RoutesToSol(t *testing.T) {
	tests := map[string]string{
		"gpt-5.6":            "gpt-5.6-sol",
		"openai/gpt-5.6":     "gpt-5.6-sol",
		"gpt5.6":             "gpt-5.6-sol",
		"gpt-5.6-high":       "gpt-5.6-sol",
		"gpt-5.6-max":        "gpt-5.6-sol",
		"gpt-5.6-2026-07-09": "gpt-5.6-sol",
		"openai/gpt-5.6-max": "gpt-5.6-sol",
	}

	for input, expected := range tests {
		t.Run(input, func(t *testing.T) {
			require.Equal(t, expected, normalizeKnownOpenAICodexModel(input))
		})
	}
}

func TestUsageBillingModelCandidates_BareGPT56IncludesSol(t *testing.T) {
	require.Equal(t,
		[]string{"gpt-5.6", "gpt-5.6-sol"},
		usageBillingModelCandidates("gpt-5.6"),
	)
	require.Equal(t,
		[]string{"openai/gpt-5.6", "gpt-5.6", "gpt-5.6-sol"},
		usageBillingModelCandidates("openai/gpt-5.6"),
	)
}

// The Codex models manifest allowlist preserves arbitrary codex-auto-* slugs.
// Normalization must therefore pass them through untouched (except for case
// and path canonicalization); collapsing them onto gpt-5.3-codex would distort
// scheduling, outbound payloads and usage billing.
func TestNormalizeKnownOpenAICodexModelCodexAutoPassthrough(t *testing.T) {
	tests := map[string]string{
		// Known fixed slug still maps to itself.
		"codex-auto-review": "codex-auto-review",
		"CODEX-Auto-Review": "codex-auto-review",
		// Unknown codex-auto-* slugs pass through as the canonical lowercase slug.
		"codex-auto-xxx":         "codex-auto-xxx",
		"codex-auto-debate-high": "codex-auto-debate-high",
		"openai/codex-auto-xxx":  "codex-auto-xxx",
		// Unrelated strings containing "codex" still fall back to gpt-5.3-codex.
		"codexxyz":           "gpt-5.3-codex",
		"codex":              "gpt-5.3-codex",
		"my-codex-tuned":     "gpt-5.3-codex",
		"gpt-5.3-codex-high": "gpt-5.3-codex",
	}

	for input, expected := range tests {
		t.Run(input, func(t *testing.T) {
			require.Equal(t, expected, normalizeKnownOpenAICodexModel(input))
		})
	}
}
