package frontend

import "embed"

// assets contains only the static production UI. No Node runtime or generated
// frontend server is required by the installed application.
//
//go:embed index.html styles.css app.js assets/*
var assets embed.FS

func Assets() embed.FS { return assets }
