package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed db/schema.sql
var schemaSQL string

func newDB(ctx context.Context) (*pgxpool.Pool, error) {
	connStr := fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s",
		getEnv("POSTGRES_USER", "quickbite"),
		getEnv("POSTGRES_PASSWORD", "quickbite"),
		getEnv("POSTGRES_HOST", "localhost"),
		getEnv("POSTGRES_PORT", "5432"),
		getEnv("POSTGRES_DB", "orders"),
	)
	return pgxpool.New(ctx, connStr)
}

func ensureSchema(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, schemaSQL)
	return err
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}