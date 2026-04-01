package main

import (
	"fmt"
	"log"
	"os"
)

func runInit(url string, useHTTP bool) {
	wd, _ := os.Getwd()
	cfg := &Config{
		URL:      url,
		HTTP:     useHTTP,
		RootPath: "", // The base of your workspace
	}

	if err := SaveConfigTo(wd, cfg); err != nil {
		log.Fatalf("Failed to initialize: %v", err)
	}

	fmt.Printf("Initialized gitty root at %s\n", wd)
	fmt.Println("You can now run 'gitty sync --path=<path>' to pull down repositories.")
}