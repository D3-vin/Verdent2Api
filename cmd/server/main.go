// Command server runs the Verdent 2API bridge (OpenAI/Anthropic-compatible
// proxy over llm-proxy.verdent.ai). All implementation lives in internal/app; this file
// only forwards to it so `go build ./cmd/server` produces the binary.
package main

import "verdent/internal/app"

func main() {
	app.Main()
}
