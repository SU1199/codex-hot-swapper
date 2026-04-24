package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"codex-hot-swapper/internal/oauth"
	"codex-hot-swapper/internal/proxy"
	"codex-hot-swapper/internal/store"
	"codex-hot-swapper/internal/switcher"
	"codex-hot-swapper/internal/usage"
	"codex-hot-swapper/internal/web"
)

const listenAddr = "127.0.0.1:2455"
const usageRefreshInterval = 5 * time.Minute

func main() {
	st, err := store.OpenDefault()
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	accountSwitcher := switcher.New(st)
	oauthSvc := oauth.New(st)
	usageSvc := usage.New(st)
	proxySvc := proxy.New(st, accountSwitcher)
	webSvc := web.New(st, oauthSvc, usageSvc)
	ctx := context.Background()
	go usageSvc.RefreshAll(ctx)
	go usageSvc.RefreshLoop(ctx, usageRefreshInterval)

	mux := http.NewServeMux()
	webSvc.Register(mux)
	proxySvc.Register(mux)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", listenAddr, err)
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	_ = openBrowser("http://" + listenAddr + "/")
	log.Printf("codex-hot-swapper running at http://%s", listenAddr)
	select {}

	_ = srv.Shutdown(context.Background())
}

func openBrowser(url string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url).Start()
	}
	if runtime.GOOS == "windows" {
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}
