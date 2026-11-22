package main

import (
	"net"
	"os"
	"time"
)

func notifySystemdReady() error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	conn, err := net.DialTimeout("unixgram", socket, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	_, err = conn.Write([]byte("READY=1"))
	return err
}
