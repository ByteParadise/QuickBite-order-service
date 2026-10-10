package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type orderItemRequest struct {
	ItemID   string `json:"itemId"`
	Quantity int    `json:"quantity"`
}

type orderRequest struct {
	RestaurantID string             `json:"restaurantId"`
	Items        []orderItemRequest `json:"items"`
}

type menuItem struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	PriceMinorUnits int64  `json:"priceMinorUnits"`
	Currency        string `json:"currency"`
	Available       bool   `json:"available"`
}

type menuResponse struct {
	RestaurantID string     `json:"restaurantId"`
	Name         string     `json:"name"`
	Items        []menuItem `json:"items"`
}

type orderLineItem struct {
	ItemID              string `json:"itemId"`
	Name                string `json:"name"`
	Quantity            int    `json:"quantity"`
	UnitPriceMinorUnits int64  `json:"unitPriceMinorUnits"`
	SubtotalMinorUnits  int64  `json:"subtotalMinorUnits"`
}

type orderResponse struct {
	ID							string					`json:"id"`
	RestaurantID    string          `json:"restaurantId"`
	Items           []orderLineItem `json:"items"`
	TotalMinorUnits int64           `json:"totalMinorUnits"`
	Currency        string          `json:"currency"`
}

var httpClient = &http.Client{Timeout: 5 * time.Second}

/* Returns restaurant service URL */
func restaurantServiceURL() string {
	if url := os.Getenv("RESTAURANT_SERVICE_URL"); url != "" {
		return url
	}
	return "http://localhost:4001"
}

/* Fetch restaurant menu */
func fetchMenu(restaurantID string) (*menuResponse, int, error) {
	// Builds the full URL to call menu endpoint
	url := fmt.Sprintf("%s/restaurants/%s/menu", restaurantServiceURL(), restaurantID)

	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close() // Make sures to close the HTTP connection.

	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("restaurant-service returned status %d", resp.StatusCode)
	}

	// reads menu res and parses it into menu struct
	var menu menuResponse
	if err := json.NewDecoder(resp.Body).Decode(&menu); err != nil {
		return nil, resp.StatusCode, err
	}
	return &menu, resp.StatusCode, nil
}

/* Write response and returns */
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("failed to write response: %v", err)
	}
}

/* Health Check
The server hands every handler a reference to a concrete incoming request struct that 
holds the method, URL, headers, and body, etc.
The ResponseWriter is an interface, so pointer doesn't work. */
func healthHandler(w http.ResponseWriter, r *http.Request) {
	// map[KeyType]ValueType
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

/* */
func makeOrdersHandler(db *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
	// Parse order request
	var req orderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	// Validate the order request
	if req.RestaurantID == "" || len(req.Items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "restaurantId and items are required"})
		return
	}
	// Verify the restaurant menu
	menu, status, err := fetchMenu(req.RestaurantID)
	if err != nil {
		log.Printf("checkout failed: could not reach restaurant-service (status %d): %v", status, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not verify restaurant menu"})
		return
	}

	// Create menuById to check the order item is valid
	menuByID := make(map[string]menuItem, len(menu.Items))
	for _, item := range menu.Items {
		menuByID[item.ID] = item
	}

	lineItems := make([]orderLineItem, 0, len(req.Items))
	var total int64
	var currency string

	for _, reqItem := range req.Items {
		if reqItem.Quantity <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid quantity for item %s", reqItem.ItemID)})
			return
		}
		item, ok := menuByID[reqItem.ItemID]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("item %s not found", reqItem.ItemID)})
			return
		}
		if !item.Available {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("item %s not available", reqItem.ItemID)})
			return
		}

		subtotal := item.PriceMinorUnits * int64(reqItem.Quantity)
		lineItems = append(lineItems, orderLineItem{
			ItemID:              item.ID,
			Name:                item.Name,
			Quantity:            reqItem.Quantity,
			UnitPriceMinorUnits: item.PriceMinorUnits,
			SubtotalMinorUnits:  subtotal,
		})
		total += subtotal
		currency = item.Currency
	}

		orderID := uuid.NewString()
		ctx := r.Context()

		tx, err := db.Begin(ctx)
		if err != nil {
			log.Printf("failed to begin transaction: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer tx.Rollback(ctx)

		_, err = tx.Exec(ctx,
			"INSERT INTO orders (id, restaurant_id, total_minor_units, currency) VALUES ($1, $2, $3, $4)",
			orderID, req.RestaurantID, total, currency,
		)
		if err != nil {
			log.Printf("failed to insert order: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		for _, item := range lineItems {
			_, err = tx.Exec(ctx,
				"INSERT INTO order_items (id, order_id, item_id, name, quantity, unit_price_minor_units, subtotal_minor_units) VALUES ($1, $2, $3, $4, $5, $6, $7)",
				uuid.NewString(), orderID, item.ItemID, item.Name, item.Quantity, item.UnitPriceMinorUnits, item.SubtotalMinorUnits,
			)
			if err != nil {
				log.Printf("failed to insert order item: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}

		if err := tx.Commit(ctx); err != nil {
			log.Printf("failed to commit order: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		writeJSON(w, http.StatusCreated, orderResponse{
			ID:              orderID,
			RestaurantID:    req.RestaurantID,
			Items:           lineItems,
			TotalMinorUnits: total,
			Currency:        currency,
		})
	}
}

func main() {
	ctx := context.Background()

	db, err := newDB(ctx)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := ensureSchema(ctx, db); err != nil {
		log.Fatalf("failed to ensure schema: %v", err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "4002"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("POST /orders", makeOrdersHandler(db))

	log.Printf("order-service listening on port %s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}