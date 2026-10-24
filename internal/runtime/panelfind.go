package runtime

import (
	"os"
	"strings"

	"github.com/dbirks/game-narrator/internal/pipeline"
)

// EnvPanelFind turns the frame-level panel finder off when set to "off",
// "0" or "false" (ADR-0172 amendment 1). Any other value, or unset, keeps it
// on: reading a box at any height is a product requirement (issue #53), the
// switch exists for an operator diagnosing a false panel.
const EnvPanelFind = "GAME_NARRATOR_PANEL_FIND"

// panelFindConfig is the session's panel-finder config: the pipeline
// defaults with Enabled decided by EnvPanelFind.
func panelFindConfig() pipeline.PanelFindConfig {
	c := pipeline.DefaultPanelFind()
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvPanelFind))) {
	case "off", "0", "false":
		c.Enabled = false
	default:
		c.Enabled = true
	}
	return c
}
