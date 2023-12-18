package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/gorilla/mux"
)

type Order struct {
	ID     string      `json:"id"`
	Status OrderStatus `json:"status"`
}

type OrderStatus string

const (
	OrderStatusPaid    OrderStatus = "PAID"
	OrderStatusPending OrderStatus = "PENDING"
	OrderStatusUnknown OrderStatus = "UNKNOWN"
)

type SchemaPatchOrder struct {
	Status OrderStatus `json:"status"`
}

const defaultDaprURL = "0.0.0.0:50001"

type Config struct {
	DaprURL string
}

type AppHandler struct {
	config *Config
	router *mux.Router
}

func NewAppHandler(config *Config) *AppHandler {
	return &AppHandler{
		config: config,
		router: mux.NewRouter(),
	}
}

func (h *AppHandler) RegisterRoutes() {
	h.router.HandleFunc("/health", h.handleHealth).Methods("GET")
	h.router.HandleFunc("/orders/{id:order-[0-9]{4}}", h.handleOrdersPut).Methods("PUT")
}

func (h *AppHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "ok\n")
}

func (h *AppHandler) handleOrdersPut(w http.ResponseWriter, r *http.Request) {
	params := mux.Vars(r)
	orderID := params["id"]

	ctx := context.Background()

	client, err := dapr.NewClientWithAddressContext(ctx, h.config.DaprURL)
	if err != nil {
		slog.Error("couldn't initialize Dapr client", "error", err)
		return
	}

	var order SchemaPatchOrder
	decoder := json.NewDecoder(r.Body)
	err = decoder.Decode(&order)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Bad request")
		return
	}

	defer client.Close()

	data := Order{ID: orderID, Status: order.Status}

	if err := client.PublishEvent(ctx, "order-pub-sub", "orders", data); err != nil {
		slog.Error("couldn't publish event", "error", err)
		return
	}

	slog.Info("sent message to orders topic", "data", data)
	fmt.Fprintf(w, "Order updated")
}

func (h *AppHandler) StartServer(address string) error {
	return http.ListenAndServe(address, h.router)
}

func main() {
	config := &Config{
		DaprURL: defaultDaprURL,
	}

	if daprURL, ok := os.LookupEnv("DAPR_URL"); ok {
		config.DaprURL = daprURL
	}

	appHandler := NewAppHandler(config)
	appHandler.RegisterRoutes()

	slog.Info("Starting server", "config", config)

	// Start the server
	if err := appHandler.StartServer(":3000"); err != nil {
		log.Fatal(err)
	}
}
