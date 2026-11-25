package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
)

func runDM8Probe() error {
	address := os.Getenv("DBSPHERE_DM8_PROBE_LISTEN")
	if address == "" {
		address = "127.0.0.1:9928"
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	cert, key := os.Getenv("DBSPHERE_DM8_PROBE_TLS_CERT"), os.Getenv("DBSPHERE_DM8_PROBE_TLS_KEY")
	if (cert == "") != (key == "") || cert == "" && !net.ParseIP(host).IsLoopback() {
		return errors.New("DM8 probe requires TLS for a non-loopback listener")
	}
	handler, err := collector.NewDM8Probe(os.Getenv("DBSPHERE_DM8_DISQL"), os.Getenv("DBSPHERE_DM8_PROBE_TOKEN"), os.Getenv("DBSPHERE_DM8_PROBE_TARGETS"))
	if err != nil {
		return err
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 13 * time.Second, IdleTimeout: 20 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if cert == "" {
		err = server.ListenAndServe()
	} else {
		err = server.ListenAndServeTLS(cert, key)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
