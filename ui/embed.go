package ui

import "embed"

// Static contains the embedded static assets
//
//go:embed static/**
var Static embed.FS
