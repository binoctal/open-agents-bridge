package config

import (
	"fmt"
	"net/url"
	"strings"
)

// official is set to "true" by goreleaser (-X ldflags) for release builds. A
// local `go build` or `go install` leaves it empty, which makes it a dev
// build. It is a string because -X can only set strings.
var official = ""

// IsOfficialBuild reports whether this binary was produced by the release
// pipeline.
func IsOfficialBuild() bool { return official == "true" }

// ControlPlane describes how a server URL relates to the official control
// plane, for display next to the target domain.
type ControlPlane struct {
	Host string
	// Environment is EnvironmentForURL's classification.
	Environment string
	// Official is true only for the exact official prod/staging hosts.
	Official bool
}

// Label is a short human-readable description of the control plane.
func (c ControlPlane) Label() string {
	if c.Official {
		return fmt.Sprintf("official %s control plane", c.Environment)
	}
	return "UNOFFICIAL control plane"
}

// ClassifyControlPlane classifies a server URL. Only the exact official hosts
// reached over TLS count as official.
func ClassifyControlPlane(raw string) ControlPlane {
	cp := ControlPlane{Host: urlHost(raw), Environment: EnvironmentForURL(raw)}
	if _, ok := officialHosts[cp.Host]; ok && secureScheme(raw) {
		cp.Official = true
	}
	return cp
}

func secureScheme(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		return true
	}
	return false
}

// CheckServerURL applies the build's server-pinning policy (5c.2). An
// official build accepts only the official prod/staging hosts over TLS; any
// other target needs unsafe=true (the explicit --unsafe-server flag). A dev
// build accepts any target. The returned ControlPlane tells the caller what
// to display; Official==false means the caller must print the loud warning.
func CheckServerURL(raw string, unsafe bool) (ControlPlane, error) {
	return checkServerURL(IsOfficialBuild(), raw, unsafe)
}

func checkServerURL(officialBuild bool, raw string, unsafe bool) (ControlPlane, error) {
	cp := ClassifyControlPlane(raw)
	if cp.Host == "" {
		return cp, fmt.Errorf("invalid server URL %q", raw)
	}
	if cp.Official || !officialBuild || unsafe {
		return cp, nil
	}
	return cp, fmt.Errorf(
		"this is an official build and only connects to the official Open Agents servers; %q is not one of them "+
			"(pass --unsafe-server only if you operate your own control plane and understand that it receives your credentials)",
		cp.Host)
}
