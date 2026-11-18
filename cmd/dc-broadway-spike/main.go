// dc-broadway-spike is a CLI helper for manual Broadway protocol checks.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gtsteffaniak/docker-computer/internal/broadway"
)

func main() {
	host := flag.String("host", "127.0.0.1:8081", "broadwayd host:port")
	w := flag.Int("width", 1280, "viewport width")
	h := flag.Int("height", 800, "viewport height")
	out := flag.String("out", "screenshot.png", "output PNG path")
	clickX := flag.Int("click-x", -1, "optional root X click")
	clickY := flag.Int("click-y", -1, "optional root Y click")
	flag.Parse()

	client := broadway.NewClient(broadway.Config{HostPort: *host, Width: *w, Height: *h})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()
	if err := client.WaitReady(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ready: %v\n", err)
		os.Exit(1)
	}
	if *clickX >= 0 && *clickY >= 0 {
		_ = client.ClickAt(int32(*clickX), int32(*clickY))
	}
	_ = client.WaitFrame(ctx, 50*time.Millisecond)
	png, err := client.ScreenshotPNG()
	if err != nil {
		fmt.Fprintf(os.Stderr, "screenshot: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, png, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(png))
}
