package main

import (
	"io/fs"

	frontendassets "github.com/Dmitbd/remote-docker/frontend"
)

func frontendAssets() fs.FS { return frontendassets.Assets() }

func applicationIcon() []byte {
	icon, _ := fs.ReadFile(frontendAssets(), "assets/app.png")
	return icon
}
