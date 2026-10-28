// Package openapi embeds the API contract so the server publishes exactly the document it was built from.
package openapi

import _ "embed"

//go:embed samuori.v1.yml
var Document []byte
