package cli

import (
	"time"

	"winc/internal/config"
	"winc/internal/engine"
	"winc/internal/platform"
	"winc/internal/server"
	"winc/internal/ui"
)

// tryContextLadder launches llama-server at the most liberal context that fits,
// silently stepping down if a size fails to load. Returns (proc, ctx) or (nil, 0).
func tryContextLadder(cfg *config.Config, hw platform.Hardware, modelPath, serverBin string, port int, serverURL, logPath string) (*server.Proc, int) {
	target := engine.ResolveContext(cfg, hw, engine.FileMB(modelPath))
	for _, ctx := range engine.ContextLadder(target) {
		args := engine.ServerArgs(cfg, hw, modelPath, port, "", ctx)
		args = append(args, engine.MTPArgs(cfg, modelPath, serverBin)...) // MTP variant -> --spec-type draft-mtp (if supported)
		proc, err := server.Start(serverBin, args, logPath)
		if err != nil {
			continue
		}
		if server.WaitReady(serverURL, "/health", 240*time.Second, proc.Dead) {
			return proc, ctx
		}
		proc.Stop() // didn't fit / failed -> try a smaller context
	}
	return nil, 0
}

// startLlamaFitting ensures a *working* engine backend and launches it. If the
// installed backend won't run here (e.g. a CUDA build whose PTX the driver is too
// old for), it silently falls back to the next backend (cuda -> vulkan -> cpu),
// re-downloading as needed, then launches at the largest context that fits.
// Returns (proc, loadedCtx) or (nil, 0).
func startLlamaFitting(cfg *config.Config, hw platform.Hardware, modelPath string, port int, serverURL, logPath string) (*server.Proc, int) {
	exclude := map[string]bool{}
	unknownCleared := false
	for {
		bin, backend, err := engine.AcquireLlamaExcluding(hw, exclude)
		if err != nil {
			ui.Err("could not get a working llama.cpp backend: %v", err)
			return nil, 0
		}
		ui.Info("loading model + waiting for server (%s)...", backendLabel(backend))
		if proc, ctx := tryContextLadder(cfg, hw, modelPath, bin, port, serverURL, logPath); proc != nil {
			return proc, ctx
		}
		// The backend didn't start at any context size -> it's incompatible here.
		if backend == "" {
			if unknownCleared {
				ui.Err("engine failed to start; see %s", logPath)
				return nil, 0
			}
			unknownCleared = true
			ui.Warn("installed engine didn't run here - switching to a compatible backend...")
			engine.ClearBinEngine()
			continue
		}
		exclude[backend] = true
		ui.Warn("%s backend isn't compatible here - trying another...", backend)
		engine.ClearBinEngine()
	}
}

func backendLabel(b string) string {
	if b == "" {
		return "installed engine"
	}
	return b + " backend"
}
