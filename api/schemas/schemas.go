// Package schemas embeds the versioned document schemas so binaries and tests share one copy.
package schemas

import _ "embed"

//go:embed buildspec.v1alpha1.json
var BuildSpecV1Alpha1 []byte

const BuildSpecV1Alpha1ID = "https://samuori.invalid/schemas/buildspec/v1alpha1.json"
