package main

import (
	"fmt"
	"sync"
	"time"

	"go.putnami.dev/http"
)

// Greeting represents a message.
type Greeting struct {
	ID        string    `json:"id"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"createdAt"`
}

// In-memory store -- replace with a real database in production.
var (
	store   = map[string]*Greeting{}
	storeMu sync.RWMutex
	seq     int
)

func listGreetings(ctx *http.Context) *http.Response {
	storeMu.RLock()
	defer storeMu.RUnlock()

	items := make([]*Greeting, 0, len(store))
	for _, g := range store {
		items = append(items, g)
	}
	return http.JSON(items)
}

func createGreeting(ctx *http.Context) *http.Response {
	var body struct {
		Message string `json:"message"`
	}
	if err := ctx.Body(&body); err != nil {
		return http.JSONStatus(400, map[string]string{"error": "invalid request body"})
	}
	if body.Message == "" {
		return http.JSONStatus(400, map[string]string{"error": "message is required"})
	}

	storeMu.Lock()
	seq++
	g := &Greeting{
		ID:        fmt.Sprintf("greet-%d", seq),
		Message:   body.Message,
		CreatedAt: time.Now().UTC(),
	}
	store[g.ID] = g
	storeMu.Unlock()

	return http.JSONStatus(201, g)
}

func getGreeting(ctx *http.Context) *http.Response {
	storeMu.RLock()
	defer storeMu.RUnlock()

	g, ok := store[ctx.Param("id")]
	if !ok {
		return http.NotFound()
	}
	return http.JSON(g)
}

func deleteGreeting(ctx *http.Context) *http.Response {
	storeMu.Lock()
	defer storeMu.Unlock()

	id := ctx.Param("id")
	if _, ok := store[id]; !ok {
		return http.NotFound()
	}
	delete(store, id)
	return http.NoContent()
}
