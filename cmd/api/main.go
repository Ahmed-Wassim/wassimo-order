package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ahmed-wassim/wassimo-order/internal/cart"
	"github.com/ahmed-wassim/wassimo-order/internal/catalog"
	"github.com/ahmed-wassim/wassimo-order/internal/config"
	"github.com/ahmed-wassim/wassimo-order/internal/order"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db pool: %v", err)
	}
	defer pool.Close()

	store := order.NewStore(pool, cart.New(cfg.CartURL), catalog.New(cfg.CatalogURL))
	h := order.NewHandlers(store, pool)

	r := http.NewServeMux()
	r.HandleFunc("/health", h.Health)
	r.HandleFunc("/ready", h.Ready)
	r.Handle("/orders", h.Routes())
	r.Handle("/orders/", h.Routes())

	log.Printf("order listening on :%s cart=%s catalog=%s", cfg.Port, cfg.CartURL, cfg.CatalogURL)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, r))
}
