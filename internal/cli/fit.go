package cli

import (
	"time"

	"winc/internal/config"
	"winc/internal/engine"
	"winc/internal/platform"
	"winc/internal/server"
	"winc/internal/ui"
)

// startLlamaFitting launches llama-server at the most liberal context that fits,
// silently falling back to smaller windows if a size fails to load (e.g. OOM).
// Returns the running process and the context that actually loaded (0 if none).
func startLlamaFitting(cfg *config.Config, hw platform.Hardware, modelPath, serverBin string, port int, serverURL, logPath string) (*server.Proc, int) {
	target := engine.ResolveContext(cfg, hw, engine.FileMB(modelPath))
	ui.Info("loading model + waiting for server...")
	for _, ctx := range engine.ContextLadder(target) {
		args := engine.ServerArgs(cfg, hw, modelPath, port, "", ctx)
		proc, err := server.Start(serverBin, args, logPath)
		if err != nil {
			continue
		}
		if server.WaitReady(serverURL, 240*time.Second, proc.Dead) {
			return proc, ctx
		}
		proc.Stop() // didn't fit / failed -> silently retry a smaller context
	}
	return nil, 0
}
