package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ahmed-wassim/wassimo-order/internal/cart"
	"github.com/ahmed-wassim/wassimo-order/internal/catalog"
	"github.com/ahmed-wassim/wassimo-order/internal/config"
	"github.com/ahmed-wassim/wassimo-order/internal/currency"
	"github.com/ahmed-wassim/wassimo-order/internal/events"
	"github.com/ahmed-wassim/wassimo-order/internal/order"
	"github.com/ahmed-wassim/wassimo-order/internal/payment"
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

	sources := []currency.Source{
		{Name: cfg.RateSource, URL: cfg.RateURL},
	}
	if cfg.FallbackURL != "" {
		sources = append(sources, currency.Source{
			Name: cfg.FallbackSource, URL: cfg.FallbackURL,
		})
	}
	if len(sources) == 0 {
		log.Fatalf("config: at least one rate source is required")
	}
	for _, s := range sources {
		if !currency.KnownSource(s.Name) {
			log.Fatalf("config: unknown rate source adapter %q", s.Name)
		}
	}
	rateClient := currency.New(cfg.BaseCurrency, cfg.QuoteCurrencies, sources, cfg.RateTimeout)

	store := order.NewStore(pool, cart.New(cfg.CartURL), catalog.New(cfg.CatalogURL),
		rateClient, cfg.BaseCurrency, cfg.ChargeCurrencies)
	h := order.NewHandlers(store, pool)

	r := http.NewServeMux()
	r.HandleFunc("/health", h.Health)
	r.HandleFunc("/ready", h.Ready)
	r.HandleFunc("/rates", h.Rates)

	payEnabled := false
	if cfg.PaymentURL != "" && cfg.PaymentAPIKey != "" {
		payClient := payment.NewClient(cfg.PaymentURL, cfg.PaymentAPIKey)
		payHandlers := payment.NewHandlers(payClient, store)
		r.HandleFunc("GET /pay-options", payHandlers.Options)
		r.HandleFunc("POST /orders/{id}/pay", payHandlers.Pay)
		r.HandleFunc("GET /orders/{id}/payment", payHandlers.Status)
		r.HandleFunc("POST /orders/{id}/payment/resolve", payHandlers.Resolve)
		r.HandleFunc("POST /orders/{id}/refunds", payHandlers.Refund)
		r.HandleFunc("POST /orders/{id}/refunds/{rid}/resolve", payHandlers.ResolveRefund)
		payEnabled = true
	}

	r.Handle("/orders", h.Routes())
	r.Handle("/orders/", h.Routes())

	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.RabbitURL != "" {
		go events.Run(appCtx, cfg.RabbitURL, store.ConsumePaymentEvent)
	}

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: r}
	go func() {
		log.Printf("order listening on :%s cart=%s catalog=%s sources=%v (base=%s) broker=%v pay=%v",
			cfg.Port, cfg.CartURL, cfg.CatalogURL, sources, cfg.BaseCurrency, cfg.RabbitURL != "", payEnabled)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	<-appCtx.Done()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdownCtx)
}
