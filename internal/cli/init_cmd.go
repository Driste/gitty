package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"gitty/internal/config"
)

func runInit(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "https://gitlab.com", "GitLab Base URL")
	useHTTP := fs.Bool("http", false, "Use HTTP(S) for cloning instead of SSH")
	if err := fs.Parse(args); err != nil {
		return 2, nil
	}

	wd, err := os.Getwd()
	if err != nil {
		return 1, fmt.Errorf("getwd: %w", err)
	}
	cfg := &config.Config{URL: *url, HTTP: *useHTTP, RootPath: "", Jobs: config.DefaultJobs}
	if err := config.Save(wd, cfg); err != nil {
		return 1, fmt.Errorf("Failed to initialize: %w", err)
	}
	fmt.Fprintf(stdout, "Initialized gitty root at %s\n", wd)
	fmt.Fprintln(stdout, "You can now run 'gitty sync --path=<path>' to pull down repositories.")
	return 0, nil
}
