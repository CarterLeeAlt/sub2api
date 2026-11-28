// Package resources contains data distributed with the server.
package resources

import _ "embed"

// ModelPricing is the fork's pricing catalog at build time.
//
//go:embed model-pricing/model_prices_and_context_window.json
var ModelPricing []byte
