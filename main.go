package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"fiatjaf.com/nostr/eventstore/boltdb"
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

	defaultDataPath := os.Getenv("DATA_PATH")
	if defaultDataPath == "" {
		defaultDataPath = "./data"
	}

	var port string
	var dataPath string
	flag.StringVar(&port, "port", defaultPort, "Port to run the relay on")
	flag.StringVar(&dataPath, "data", defaultDataPath, "Data directory for the relay (BoltDB)")
	flag.Parse()

	if err := os.MkdirAll(dataPath, 0755); err != nil {
		fmt.Printf("failed to create data directory: %v\n", err)
		return
	}

	dbPath := filepath.Join(dataPath, "relay.db")

	// Initialize BoltDB backend
	db := boltdb.BoltBackend{Path: dbPath}
	if err := db.Init(); err != nil {
		fmt.Printf("failed to init boltdb: %v\n", err)
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
