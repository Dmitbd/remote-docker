package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/Dmitbd/remote-docker/internal/config"
)

// UpgradeConfig persists the current schema while the caller owns the
// cross-session desktop instance gate. The state transaction prevents a
// current-version writer from racing the migration itself.
func UpgradeConfig(ctx context.Context, configPath string) error {
	if !filepath.IsAbs(configPath) {
		return errors.New("config upgrade path must be absolute")
	}
	store := config.Store{Path: configPath}
	transactions := newConfigTransactions(configPath)
	return transactions.RunContext(ctx, func() error {
		cfg, migration, err := store.LoadWithMigration()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if migration.FromVersion == config.CurrentSchemaVersion {
			return nil
		}
		cfg.SchemaVersion = config.CurrentSchemaVersion
		return store.Save(cfg)
	})
}
