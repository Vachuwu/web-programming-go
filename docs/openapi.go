// Package docs embeds the API description served by the development server.
package docs

import _ "embed"

// OpenAPI is the OpenAPI 3.1 document for the public HTTP API.
//
//go:embed openapi.yaml
var OpenAPI []byte
