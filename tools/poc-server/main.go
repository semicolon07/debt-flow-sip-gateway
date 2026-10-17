package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	address := flag.String("listen", "127.0.0.1:8765", "loopback address for the POC server")
	directory := flag.String("dir", filepath.Join("test", "poc"), "directory containing the POC files")
	flag.Parse()

	if *address != "127.0.0.1:8765" {
		log.Fatal("the POC server is intentionally restricted to 127.0.0.1:8765")
	}
	root, err := filepath.Abs(*directory)
	if err != nil {
		log.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "sip-gateway-poc.html")); err != nil || info.IsDir() {
		log.Fatalf("POC file not found under %s", root)
	}

	files := http.FileServer(http.Dir(root))
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Pragma", "no-cache")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self' http: https: ws: wss:; media-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if request.URL.Path == "/" {
			http.Redirect(writer, request, "/sip-gateway-poc.html", http.StatusTemporaryRedirect)
			return
		}
		files.ServeHTTP(writer, request)
	})

	server := &http.Server{
		Addr:              *address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	fmt.Printf("Debt Flow SIP Gateway POC: http://%s/sip-gateway-poc.html\n", *address)
	log.Fatal(server.ListenAndServe())
}
