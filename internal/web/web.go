// Package web embeds the admin UI assets served by model-router.
package web

import _ "embed"

// IndexHTML is the self-contained admin configuration page.
//
//go:embed index.html
var IndexHTML []byte

// LoginHTML is the standalone admin login page (§24.7 D4).
//
//go:embed login.html
var LoginHTML []byte

// FaviconICO is the multi-size browser icon (16/32/48/64/128/256, §24.13 K3).
//
//go:embed favicon.ico
var FaviconICO []byte

// LogoSVG is the vector brand mark shown in the admin header (§24.13 K3).
//
//go:embed logo.svg
var LogoSVG []byte
