package api

// The tailnet address detection for UX-13 (ADR-0110 §4): the box's
// Tailscale address, so the settings page and the system-info surface can
// show the tailnet URL — "the Tailscale address for reaching the web UI over
// the tailnet" — copied from the running system, so it always matches the
// appliance.
//
// The detection runs ONCE, at process startup (cmd/game-narrator detects
// before the listener comes up and injects the cached value through
// WithTailnet): the interface scan is a netlink read, and the `tailscale ip`
// fallback is the one subprocess the design permits — bounded, cached, and
// never at request time. The value then crosses exactly two boundaries, both
// local: the settings page and GET /api/v1/system/info. It is never sent to
// any external service — the update checker, the provider requests and the
// support bundle receive no seam that carries it, which is the "never
// external" rule made structural rather than promised.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// tailnetCGNAT is Tailscale's default address range (100.64.0.0/10, the
// CGNAT block IANA reserved for this purpose): an interface address inside
// it is the tailnet's half of the detection, found without the CLI.
var tailnetCGNAT = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// tailnetIPTimeout bounds the one `tailscale ip` call the fallback makes:
// a wedged CLI must degrade to "not detected", not hold the startup.
const tailnetIPTimeout = 2 * time.Second

// The detection source words (ADR-0110 §4): the interface scan's word and
// the tailscale-ip fallback's word. The contract's enum vocabulary is
// pinned to these in openapi_enums_test.go — the writer and the document
// cannot drift.
const (
	tailnetSourceInterface = "interface"
	tailnetSourceTailscale = "tailscale"
)

// TailnetDeps is the seam DetectTailnet runs through (ADR-0110 §4): the
// interface lister, the per-interface address reader and the `tailscale ip`
// reader are injectable, so the tests supply synthetic interfaces and a
// scripted CLI answer instead of the host's network namespace. A nil
// function takes the production behaviour: net.Interfaces, the
// interface's own Addrs, and the bounded `tailscale ip` subprocess.
type TailnetDeps struct {
	// Interfaces lists the machine's network interfaces (net.Interfaces).
	Interfaces func() ([]net.Interface, error)
	// Addrs reports one interface's addresses (net.Interface.Addrs).
	Addrs func(ni net.Interface) ([]net.Addr, error)
	// TailscaleIP runs `tailscale ip` once and returns its answer (the
	// node's tailnet IP). nil runs the default bounded subprocess.
	TailscaleIP func(ctx context.Context) (string, error)
	// Port is the bound HTTP port the URL is built with (the listen
	// address's port; 0 answers without a URL).
	Port int
}

func (d TailnetDeps) interfaces() func() ([]net.Interface, error) {
	if d.Interfaces != nil {
		return d.Interfaces
	}
	return net.Interfaces
}

func (d TailnetDeps) addrs() func(net.Interface) ([]net.Addr, error) {
	if d.Addrs != nil {
		return d.Addrs
	}
	return func(ni net.Interface) ([]net.Addr, error) { return ni.Addrs() }
}

func (d TailnetDeps) tailscaleIP() func(ctx context.Context) (string, error) {
	if d.TailscaleIP != nil {
		return d.TailscaleIP
	}
	return defaultTailscaleIP
}

// TailnetView is the detection's answer (the JSON shape both surfaces
// carry): present and the address when the tailnet was found — with the
// URL built from the bound port and the source naming which half of the
// detection found it — or present=false with an explicit note when it was
// not (UX-13: the "not configured" state, never a blank field).
type TailnetView struct {
	// Present is true when a tailnet address was detected.
	Present bool `json:"present"`
	// Address is the tailnet IPv4 ("" when absent).
	Address string `json:"address,omitempty"`
	// Port is the bound HTTP port the URL points at (0 when none).
	Port int `json:"port,omitempty"`
	// URL is the tailnet URL (http://address:port), the settings page's
	// display value; "" when absent or when no port is known.
	URL string `json:"url,omitempty"`
	// Source names the detection half that found it: "interface" (a
	// 100.64.0.0/10 address on a machine interface) or "tailscale" (the
	// `tailscale ip` fallback). "" when absent.
	Source string `json:"source,omitempty"`
	// Note is the explicit absent-state text (UX-13's "not configured",
	// never a blank field) and, when present, how the address was found.
	Note string `json:"note,omitempty"`
}

// DetectTailnet runs the two-half detection (ADR-0110 §4) and returns the
// cached answer the Server serves for its life. The order is deliberate:
// the interface scan needs no subprocess and finds the default-range
// address directly, so the CLI fallback runs only when the scan found
// nothing.
func DetectTailnet(ctx context.Context, d TailnetDeps) TailnetView {
	absent := func(why string) TailnetView {
		return TailnetView{Present: false, Note: why}
	}

	// Half one: a 100.64.0.0/10 (CGNAT) IPv4 on any machine interface.
	// The first such address wins; interface order is stable enough for
	// one appliance with one tailnet interface, and the value is the IP,
	// not the interface name (the note names the interface for the
	// operator, nothing more).
	ifaces, err := d.interfaces()()
	if err == nil {
		addrs := d.addrs()
		for _, ni := range ifaces {
			as, aerr := addrs(ni)
			if aerr != nil {
				continue // an interface that cannot be read is skipped, not fatal
			}
			for _, a := range as {
				ipnet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				ip := ipnet.IP
				if v4 := ip.To4(); v4 != nil && tailnetCGNAT.Contains(v4) {
					url := ""
					if d.Port > 0 {
						url = fmt.Sprintf("http://%s:%d", v4.String(), d.Port)
					}
					return TailnetView{
						Present: true,
						Address: v4.String(),
						Port:    d.Port,
						URL:     url,
						Source:  tailnetSourceInterface,
						Note:    fmt.Sprintf("detected at startup from a 100.64.0.0/10 address on interface %s", ni.Name),
					}
				}
			}
		}
	}

	// Half two: the `tailscale ip` CLI, when available and when the scan
	// found nothing. The one subprocess the design permits — bounded, at
	// startup, cached. A missing binary or a failed run degrades to the
	// absent answer with the reason, never a panic and never a retry at
	// request time.
	if read := d.tailscaleIP(); read != nil {
		ctx, cancel := context.WithTimeout(ctx, tailnetIPTimeout)
		ipStr, err := read(ctx)
		cancel()
		if err == nil {
			if v4 := net.ParseIP(strings.TrimSpace(ipStr)).To4(); v4 != nil {
				url := ""
				if d.Port > 0 {
					url = fmt.Sprintf("http://%s:%d", v4.String(), d.Port)
				}
				return TailnetView{
					Present: true,
					Address: v4.String(),
					Port:    d.Port,
					URL:     url,
					Source:  tailnetSourceTailscale,
					Note:    "detected at startup from `tailscale ip` (no 100.64.0.0/10 interface address was found)",
				}
			}
			return absent("Tailscale is not configured on this machine: no 100.64.0.0/10 interface address, and `tailscale ip` reported no usable IP")
		}
		var xerr *exec.Error
		if errors.As(err, &xerr) {
			return absent("Tailscale is not configured on this machine: no 100.64.0.0/10 interface address, and the `tailscale` binary is not available")
		}
		return absent(fmt.Sprintf("Tailscale is not configured on this machine: no 100.64.0.0/10 interface address, and `tailscale ip` could not be run (%v)", err))
	}
	return absent("Tailscale is not configured on this machine: no 100.64.0.0/10 interface address was found and no `tailscale ip` fallback is available")
}

// defaultTailscaleIP is the production `tailscale ip` reader (ADR-0110 §4):
// the one bounded subprocess the detection permits, run at startup only.
// A missing binary answers the wrapped exec.ErrNotFound, which the caller
// turns into the absent note.
func defaultTailscaleIP(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, tailnetIPTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "ip").Output()
	if err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) {
			return "", ee // the binary is not installed
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TailnetPort parses the port out of a bound listen address (the "host:port"
// of cfg.Listen; a bare port is accepted too). A malformed address answers
// 0, which the view renders as "no URL" rather than an invented one.
func TailnetPort(listen string) int {
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		if s := strings.TrimSpace(listen); s != "" && !strings.Contains(s, ".") {
			portStr = s // a bare port
		} else {
			return 0
		}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}
