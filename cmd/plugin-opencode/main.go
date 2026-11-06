package main

import (
	runtime "github.com/slidebolt/sb-runtime"

	"github.com/slidebolt/plugin-opencode/app"
)

func main() {
	runtime.Run(app.New())
}
