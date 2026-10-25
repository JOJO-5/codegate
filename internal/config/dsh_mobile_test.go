package config

import "testing"

func TestDSHMobileLayoutRequiresExplicitOptIn(t *testing.T) {
	c := Defaults()
	if !c.DisableDSHMobileUI {
		t.Fatal("native DSH layout must be the default")
	}
	if err := c.applyEnv(func(k string) string {
		if k == "CODEGATE_DSH_MOBILE_UI" {
			return "true"
		}
		return ""
	}); err != nil || c.DisableDSHMobileUI {
		t.Fatal("operator cannot opt into compatibility")
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
