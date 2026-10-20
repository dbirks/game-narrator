package api

// The /settings "Network" section (docs/design/plum-peach product-notes UX-13,
// ADR-0109): the tailnet URL — the Tailscale address for reaching this
// appliance's web UI over the tailnet. The appliance has no screen of its
// own, so the URL is what the family types on the laptop beside the TV, and
// it must be copied from the running system so the display always matches
// the appliance.
//
// Resolution, in order:
//
//  1. GAME_NARRATOR_TAILNET_URL (source "env"): the operator's pin — the
//     appliance's env file can carry the stable *.ts.net name, and the pin
//     wins so the display never depends on CLI availability.
//  2. the live `tailscale status --json` self record (source "tailscale"):
//     the machine's MagicDNS name (Self.DNSName) with the bound listen port.
//     The probe is the audio package's subprocess posture (ADR-0023): the
//     binary is looked up on PATH, runs under a bounded context, and any
//     failure is an honest "not configured" — never an invented URL.
//  3. otherwise tailnet_url is null with a note naming the gap. The UI
//     renders the note verbatim: an explicit not-configured state, not a
//     blank field.
//
// The URL is an address, not a credential (docs/spec/14): the admin-gated
// settings route already renders the bound listen address, and the value
// crosses this boundary only from the machine's own tailscale state or the
// operator's own environment. It never appears in logs, the public status
// stream, or system-info.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
)

// EnvTailnetURL is the operator's pin of the appliance's tailnet URL
// (ADR-0109). When set, the settings page renders exactly this value — the
// stable *.ts.net name the operator wrote — regardless of what the tailscale
// CLI answers.
const EnvTailnetURL = "GAME_NARRATOR_TAILNET_URL"

// tailnetProbeTimeout bounds the live probe: the settings page is an operator
// surface, and a wedged CLI must not hold the response open.
const tailnetProbeTimeout = 5 * time.Second

// networkView is the GET /api/v1/settings "network" section.
type networkView struct {
	// TailnetURL is the URL to reach this appliance's web UI over the
	// tailnet (the Tailscale address), or null when not configured.
	TailnetURL *string `json:"tailnet_url"`
	// Source names where the URL came from: "env" (the GAME_NARRATOR_TAILNET_URL
	// pin) or "tailscale" (the live status record). Null alongside a null URL.
	Source *string `json:"source"`
	// Note is the one-line context: the pin's variable, the derivation, or
	// the reason the URL is null. The UI renders it verbatim.
	Note string `json:"note"`
}

func stringSource(s string) *string { return &s }

// tailscaleStatusProbe stands in for the live probe in handler tests: the
// same indirection osLookupEnv gives the other sections (a test points the
// page at a fixture answer without touching PATH).
var tailscaleStatusProbe = runTailscaleStatus

// tailscaleStatusSelf is the Self record of `tailscale status --json`: the
// machine's own entry. Only the DNS name is read — the MagicDNS name is the
// stable address the tailnet resolves, and the record's IPs and login name
// are host details that stay server-side (docs/spec/08's system-info rule).
type tailscaleStatusSelf struct {
	DNSName string `json:"DNSName"`
}

// runTailscaleStatus runs `tailscale status --json` under a bounded context
// and returns the Self record. Every failure mode — no binary, the daemon
// not running, unparseable output, an empty DNS name — answers a nil self
// with the reason, so the caller renders "not configured" with the machine's
// own words instead of guessing.
func runTailscaleStatus(ctx context.Context) (*tailscaleStatusSelf, string) {
	ctx, cancel := context.WithTimeout(ctx, tailnetProbeTimeout)
	defer cancel()

	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return nil, "no tailscale CLI on PATH"
	}
	cmd := exec.CommandContext(ctx, bin, "status", "--json")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, "the tailscale status probe timed out"
		}
		return nil, "the tailscale CLI did not answer (is tailscaled running?)"
	}
	var record struct {
		Self tailscaleStatusSelf `json:"Self"`
	}
	if err := json.Unmarshal(out, &record); err != nil {
		return nil, "the tailscale status output was not parseable"
	}
	if strings.TrimSpace(record.Self.DNSName) == "" {
		return nil, "tailscale has no MagicDNS name for this machine (it is not on a tailnet)"
	}
	return &record.Self, ""
}

// listenPortOf resolves the port the web UI binds, for the tailnet URL the
// appliance is reachable on. The bound address (WithListen) wins; without
// it, the environment's own resolution (the same seam settingsSecurity
// uses), so the section and the security section cannot disagree.
func (s *Server) listenPortOf() (int, bool) {
	for _, candidate := range []string{s.listenAddr, bootstrap.ResolveListen("", osLookupEnv)} {
		if candidate == "" {
			continue
		}
		host, port, err := net.SplitHostPort(candidate)
		if err != nil || host == "" {
			continue
		}
		n, err := strconv.Atoi(port)
		if err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// settingsNetwork builds the network section (ADR-0109).
func (s *Server) settingsNetwork(ctx context.Context) networkView {
	pin, _ := osLookupEnv(EnvTailnetURL)
	if pin := strings.TrimSpace(pin); pin != "" {
		return networkView{
			TailnetURL: stringSource(pin),
			Source:     stringSource("env"),
			Note:       fmt.Sprintf("Pinned by %s — the operator's stable tailnet name (the environment override beats the live probe).", EnvTailnetURL),
		}
	}
	self, reason := tailscaleStatusProbe(ctx)
	if self == nil {
		return networkView{
			TailnetURL: nil,
			Source:     nil,
			Note:       "Tailscale is not configured: " + reason + ". Set " + EnvTailnetURL + " to pin the URL, or run tailscaled.",
		}
	}
	port, ok := s.listenPortOf()
	if !ok {
		return networkView{
			TailnetURL: nil,
			Source:     nil,
			Note: "Tailscale reports this machine (" + self.DNSName + ") but the bound listen port could not be resolved — " +
				"set " + bootstrap.EnvListen + " or " + EnvTailnetURL + ".",
		}
	}
	return networkView{
		TailnetURL: stringSource(fmt.Sprintf("http://%s:%d", self.DNSName, port)),
		Source:     stringSource("tailscale"),
		Note:       fmt.Sprintf("Derived from the live tailscale status (Self.DNSName %s) with the bound listen port.", self.DNSName),
	}
}
