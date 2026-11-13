// Command probe-upstream is the probe harness's stand-in for a product
// upstream.
//
// It exists so that configs/.probe/run.sh can observe what the Gateway
// actually sent — which upstream answered, under which Host, with which
// X-Tenant-ID and X-Forwarded-* — rather than inferring the answer from a
// status code.
//
// This is deliberately NOT cmd/mockproduct. That is an M9 deliverable and will
// carry the full per-product surface (a JSON backend and an HTML frontend with
// a history fallback). This is the smallest thing that answers the question M5
// asks, and it should be deleted when cmd/mockproduct lands.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// streamGap is how long /__stream waits between its two events. A client that
// receives the first one before this elapses has proof the response was not
// buffered anywhere along the path.
const streamGap = 2 * time.Second

func main() {
	name := flag.String("name", "upstream", "identity this upstream reports")
	addr := flag.String("addr", "127.0.0.1:0", "address to listen on")
	flag.Parse()

	mux := http.NewServeMux()

	// /__echo reports exactly what arrived, so a probe can assert the header
	// contract rather than trust a 200.
	mux.HandleFunc("/__echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"upstream": *name,
			"method":   r.Method,
			"path":     r.URL.Path,
			"query":    r.URL.RawQuery,
			"host":     r.Host,
			"headers": map[string]string{
				"X-Tenant-ID":       r.Header.Get("X-Tenant-ID"),
				"X-Request-ID":      r.Header.Get("X-Request-ID"),
				"X-Forwarded-For":   r.Header.Get("X-Forwarded-For"),
				"X-Forwarded-Host":  r.Header.Get("X-Forwarded-Host"),
				"X-Forwarded-Proto": r.Header.Get("X-Forwarded-Proto"),
			},
		})
	})

	// /__stream emits an event, waits, then emits another.
	mux.HandleFunc("/__stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		emit := func(event string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, *name)
			_ = http.NewResponseController(w).Flush()
		}

		emit("first")

		select {
		case <-time.After(streamGap):
		case <-r.Context().Done():
			return
		}

		emit("second")
	})

	mux.HandleFunc("/__health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Everything else identifies the upstream and echoes the request, which is
	// all the §90 routing rows need. The query and a digest of the body are
	// reported because "the path was not rewritten" and "the body arrived
	// intact" are claims a status code cannot support.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		fmt.Fprintf(w, "upstream=%s host=%s path=%s query=%s\n",
			*name, r.Host, r.URL.Path, r.URL.RawQuery)

		if r.Body == nil {
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			fmt.Fprintf(w, "body_error=%v\n", err)
			return
		}
		if len(body) > 0 {
			sum := sha256.Sum256(body)
			fmt.Fprintf(w, "body_bytes=%d body_sha256=%x\n", len(body), sum[:8])
		}
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Printf("upstream %s listening on %s\n", *name, *addr)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
