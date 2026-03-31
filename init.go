package main

import (
	"fmt"
	"log"
	"os"
)

func runInit(url string, useHTTP bool) {
	if _, err := os.Stat(ConfigPath); err == nil {
		fmt.Printf("Error: gitty is already initialized in this directory (%s exists).\n", ConfigPath)
		os.Exit(1)
	}

	cfg := &Config{
		URL:  url,
		HTTP: useHTTP,
	}

	if err := SaveConfig(cfg); err != nil {
		log.Fatalf("Failed to initialize gitty: %v", err)
	}

	fmt.Printf("Successfully initialized gitty in %s\n", ConfigPath)
	fmt.Printf("URL: %s\nHTTP: %t\n", cfg.URL, cfg.HTTP)
	fmt.Println("You can now run 'gitty sync --group=<path>' to pull down repositories.")
}
