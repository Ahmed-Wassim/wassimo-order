package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Port        string
	Env         string
	DatabaseURL string
	CartURL     string
	CatalogURL  string

	RateURL          string
	RateTimeout      time.Duration
	BaseCurrency     string
	QuoteCurrencies  []string
	ChargeCurrencies []string

	RateSource     string
	FallbackURL    string
	FallbackSource string

	RabbitURL string
}

func Load() (Config, error) {
	if os.Getenv("ENV") != "prod" {
		_ = godotenv.Load()
	}

	cfg := Config{
		Port:        os.Getenv("PORT"),
		Env:         os.Getenv("ENV"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		CartURL:     os.Getenv("CART_SERVICE_URL"),
		CatalogURL:  os.Getenv("CATALOG_SERVICE_URL"),

		RateURL:          getEnv("RATE_PRIMARY_URL", "https://api.frankfurter.dev"),
		RateTimeout:      durationEnv("RATE_TIMEOUT_MS", 2000),
		BaseCurrency:     strings.ToUpper(strings.TrimSpace(getEnv("RATE_BASE", "EGP"))),
		QuoteCurrencies:  listEnv("RATE_QUOTES"),
		ChargeCurrencies: listEnv("CHARGE_CURRENCIES"),
		RateSource:       getEnv("RATE_SOURCE", "frankfurter"),
		FallbackURL:      fallbackEnv(),
		FallbackSource:   getEnv("RATE_FALLBACK_SOURCE", "er-api"),

		RabbitURL: os.Getenv("RABBITMQ_URL"),
	}
	if len(cfg.ChargeCurrencies) == 0 {
		cfg.ChargeCurrencies = uniqueUpper(
			append([]string{cfg.BaseCurrency}, cfg.QuoteCurrencies...))
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.CartURL == "" {
		return Config{}, fmt.Errorf("CART_SERVICE_URL is required")
	}
	if cfg.CatalogURL == "" {
		return Config{}, fmt.Errorf("CATALOG_SERVICE_URL is required")
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func listEnv(key string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	return uniqueUpper(strings.Split(raw, ","))
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

// fallbackEnv — "off" disables the fallback entirely.
func fallbackEnv() string {
	raw := strings.TrimSpace(os.Getenv("RATE_FALLBACK_URL"))
	if strings.EqualFold(raw, "off") {
		return ""
	}
	if raw == "" {
		return "https://open.er-api.com"
	}
	return raw
}

func uniqueUpper(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
