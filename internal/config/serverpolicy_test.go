package config

import "testing"

func TestOfficialBuildPinsServerURL(t *testing.T) {
	reject := []string{
		"https://evil.com/?staging",
		"https://localhost.evil.com",
		"https://api.openagents.top.evil.com",
		"https://evil.com/api.openagents.top",
		"http://localhost:8787",
		"http://127.0.0.1:8989",
		"http://api.openagents.top", // official host without TLS
		"ws://api.openagents.top",
		"not a url",
		"",
	}
	for _, raw := range reject {
		if _, err := checkServerURL(true, raw, false); err == nil {
			t.Errorf("official build must reject %q", raw)
		}
	}
	accept := map[string]string{
		"https://api.openagents.top":          "production",
		"wss://api.openagents.top":            "production",
		"https://api-staging.openagents.top/": "staging",
	}
	for raw, env := range accept {
		cp, err := checkServerURL(true, raw, false)
		if err != nil || !cp.Official || cp.Environment != env {
			t.Errorf("official build must accept %q as official %s: cp=%+v err=%v", raw, env, cp, err)
		}
	}
}

func TestDevBuildAcceptsLoopbackAndMarksUnofficial(t *testing.T) {
	for _, raw := range []string{"http://localhost:8787", "http://127.0.0.1:8989", "http://[::1]:8989", "https://self-hosted.example.com"} {
		cp, err := checkServerURL(false, raw, false)
		if err != nil {
			t.Errorf("dev build must accept %q: %v", raw, err)
		}
		if cp.Official {
			t.Errorf("%q must be marked unofficial", raw)
		}
	}
}

func TestUnsafeServerOverrideStillMarksUnofficial(t *testing.T) {
	cp, err := checkServerURL(true, "https://evil.com", true)
	if err != nil {
		t.Fatalf("--unsafe-server must allow: %v", err)
	}
	if cp.Official || cp.Label() != "UNOFFICIAL control plane" {
		t.Fatalf("must be marked unofficial: %+v", cp)
	}
}

func TestDefaultTestBuildIsNotOfficial(t *testing.T) {
	if IsOfficialBuild() {
		t.Fatal("a build without the ldflag must be a dev build")
	}
}
