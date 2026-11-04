package db

import (
	"strings"
	"testing"
)

func TestPoolConfigForcesUTC(t *testing.T) {
	tests := []string{
		"postgres://u:p@localhost:5432/db",
		"postgres://u:p@localhost:5432/db?timezone=Europe/Istanbul",
		"postgres://u:p@localhost:5432/db?TimeZone=Europe/Istanbul",
		"host=localhost user=u password=p dbname=db timezone=Europe/Istanbul",
	}
	for _, url := range tests {
		cfg, err := poolConfig(url, "composer-api")
		if err != nil {
			t.Fatalf("poolConfig(%q): %v", url, err)
		}
		params := cfg.ConnConfig.RuntimeParams
		var tz []string
		for k, v := range params {
			if strings.EqualFold(k, "timezone") {
				tz = append(tz, k+"="+v)
			}
		}
		if len(tz) != 1 || tz[0] != "timezone=UTC" {
			t.Errorf("%q: timezone params = %v, want [timezone=UTC]", url, tz)
		}
		if params["application_name"] != "composer-api" {
			t.Errorf("%q: application_name = %q", url, params["application_name"])
		}
	}
}

func TestPoolConfigInvalidURLDoesNotLeakPassword(t *testing.T) {
	_, err := poolConfig("postgres://user:s3cr3tpw@localhost:notaport/db", "x")
	if err == nil {
		t.Fatal("poolConfig succeeded, want error")
	}
	if strings.Contains(err.Error(), "s3cr3tpw") {
		t.Errorf("error leaks password: %v", err)
	}
}
