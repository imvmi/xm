package archive

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestChromeAvailabilityOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("set XM_BROWSER_TEST=1 to run real-Chrome availability test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := DefaultConfig()
	info, err := CheckChrome(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Headless || !strings.Contains(info.Product, "Chrome/") {
		t.Fatalf("unexpected Chrome details: %+v", info)
	}
}
