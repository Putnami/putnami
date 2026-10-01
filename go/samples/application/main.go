package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	golib "go.putnami.dev/examples/library"
)

func main() {
	// Demonstrate library usage
	fmt.Println("Go App with Library Example")
	fmt.Println("============")

	examples := []string{"hello", "world", "racecar", "putnami"}

	for _, example := range examples {
		reversed := golib.Reverse(example)
		capitalized := golib.Capitalize(example)
		isPal := golib.IsPalindrome(example)

		fmt.Printf("Input: %s\n", example)
		fmt.Printf("  Reversed: %s\n", reversed)
		fmt.Printf("  Capitalized: %s\n", capitalized)
		fmt.Printf("  Is Palindrome: %v\n", isPal)
		fmt.Println()
	}

	// Start HTTP server if PORT is set
	port := os.Getenv("PORT")
	if port == "" {
		port = "3920"
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, err := fmt.Fprintf(w, `
    Go App with Library Example
    ============
    Library functions:
    Reverse('hello') = %s
    Capitalize('world') = %s
    IsPalindrome('racecar') = %v
    `, golib.Reverse("hello"), golib.Capitalize("world"), golib.IsPalindrome("racecar"))

		if err != nil {
			fmt.Fprintf(os.Stderr, "Error writing response: %v\n", err)
			os.Exit(1)
		}
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           nil, // or your mux
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	log.Println("server listening on", srv.Addr)
	<-done
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	if err := srv.Shutdown(ctx); err != nil {
		cancel()
		log.Fatalf("shutdown error: %v", err)
	}
	cancel()
}
