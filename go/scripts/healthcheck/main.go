// Command kagent-healthcheck GETs a URL and exits 0 only on a 2xx response.
//
// It exists so the controller dev image's dev_sync health/smoke commands do
// not depend on curl or wget being present in the base Go toolchain image;
// the controller's own module already builds happily in that image, so a
// tiny Go binary is the dependency-free option.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	url := "http://127.0.0.1:8083/health"
	if len(os.Args) > 1 {
		url = os.Args[1]
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kagent-healthcheck:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "kagent-healthcheck: %s -> %s\n", url, resp.Status)
		os.Exit(1)
	}
	fmt.Printf("kagent-healthcheck: %s -> %s\n", url, resp.Status)
}
