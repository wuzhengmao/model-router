// Package web embeds the admin UI assets served by model-router.
package web

import _ "embed"

// IndexHTML is the self-contained admin configuration page.
//
//go:embed index.html
var IndexHTML []byte
