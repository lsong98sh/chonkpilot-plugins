// Package web embeds the built H5 application served by the remote server.
package web

import "embed"

// Dist holds the built H5 site; index.html and hashed assets live under dist/.
//
//go:embed all:dist
var Dist embed.FS
