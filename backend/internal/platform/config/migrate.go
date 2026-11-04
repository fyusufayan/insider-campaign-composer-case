package config

import (
	"errors"
	"fmt"
	"strings"
)

// Migrate is the validated configuration of the migrate command.
type Migrate struct {
	DatabaseURL Secret
}

// LoadMigrate reads the migrate command's configuration through getenv.
func LoadMigrate(getenv func(string) string) (Migrate, error) {
	cfg := Migrate{DatabaseURL: Secret(strings.TrimSpace(getenv("DATABASE_URL")))}
	if cfg.DatabaseURL == "" {
		return Migrate{}, fmt.Errorf("invalid configuration: %w", errors.New("DATABASE_URL is required"))
	}
	return cfg, nil
}
