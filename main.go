package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"fiatjaf.com/nostr/eventstore/lmdb"
	"fiatjaf.com/nostr/khatru"
)

func main() {
	relay := khatru.NewRelay()

	// Set basic NIP-11 relay metadata
	relay.Info.Name = "serial-killer"
	relay.Info.Description = "A bare bones khatru relay"
	relay.Info.Software = "https://github.com/fiatjaf/khatru"
	relay.Info.Version = "0.0.1"

	defaultPort := os.Getenv("PORT")
	if defaultPort == "" {
		defaultPort = "3334"
	}

	defaultDbPath := os.Getenv("DB_PATH")
	if defaultDbPath == "" {
		defaultDbPath = "./db"
	}

	var port string
	var dbPath string
	flag.StringVar(&port, "port", defaultPort, "Port to run the relay on")
	flag.StringVar(&dbPath, "db-path", defaultDbPath, "Path to the LMDB database directory")
	flag.Parse()

	// Initialize LMDB backend
	db := lmdb.LMDBBackend{Path: dbPath}
	if err := db.Init(); err != nil {
		fmt.Printf("failed to init lmdb: %v\n", err)
		return
	}
	defer db.Close()

	relay.UseEventstore(&db, 500)

	// Start HTTP server
	fmt.Printf("Relay running on :%s\n", port)
	if err := http.ListenAndServe(":"+port, relay); err != nil {
		fmt.Printf("server error: %v\n", err)
	}
}
