package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
)

func runContainerAgent() error {
	if runtime.GOOS != "linux" {
		return errors.New("container agent requires Linux")
	}
	bind := os.Getenv("DBSPHERE_CONTAINER_AGENT_ADDRESS")
	if bind == "" {
		bind = "127.0.0.1:9918"
	}
	directory := os.Getenv("DBSPHERE_CGROUP_ROOT")
	if directory == "" {
		directory = "/sys/fs/cgroup"
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	handler, err := collector.ContainerCgroupHandler(root.FS(), os.Getenv("DBSPHERE_CONTAINER_AGENT_TOKEN"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: bind, Handler: handler, ReadHeaderTimeout: 3 * time.Second, WriteTimeout: 6 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
