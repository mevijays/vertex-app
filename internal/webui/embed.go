package webui

import "embed"

// Files contains the browser application served by the Go binary.
//
//go:embed assets/*
var Files embed.FS
