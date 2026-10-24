package config

import "testing"

func TestDSHMobileLayoutCanBeDisabled(t *testing.T) {
	c := Defaults()
	if c.DisableDSHMobileUI {
		t.Fatal("compatibility unexpectedly disabled by default")
	}
	if err := c.applyEnv(func(k string) string {
		if k == "CODEGATE_DSH_MOBILE_UI" {
			return "false"
		}
		return ""
	}); err != nil || !c.DisableDSHMobileUI {
		t.Fatal("operator cannot disable adaptation")
	}
	if err := c.applyEnv(func(k string) string {
		if k == "CODEGATE_DSH_MOBILE_UI" {
			return "invalid"
		}
		return ""
	}); err == nil {
		t.Fatal("invalid toggle silently accepted")
	}
}
