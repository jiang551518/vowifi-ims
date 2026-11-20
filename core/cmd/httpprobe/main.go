// httpprobe fetches each URL and prints status and latency.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	c := &http.Client{Timeout: 8 * time.Second}
	for _, u := range os.Args[1:] {
		t := time.Now()
		r, err := c.Get(u)
		if err != nil {
			fmt.Printf("%-40s FAIL %v\n", u, err)
			continue
		}
		r.Body.Close()
		fmt.Printf("%-40s HTTP %d %v\n", u, r.StatusCode, time.Since(t).Round(time.Millisecond))
	}
}
