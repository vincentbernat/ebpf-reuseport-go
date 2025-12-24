// SPDX-FileCopyrightText: 2025 Free Mobile

package main

import (
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUDPWorkerBalancing(t *testing.T) {
	workers := 10

	// Fro each worker, setup the listening sockets with SO_REUSEPORT
	var fds []uintptr
	var conns []*net.UDPConn
	var err error
	listeningAddr := "127.0.0.1:0"
	listenConfig := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			c.Control(func(fd uintptr) {
				err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
				fds = append(fds, fd)
			})
			return err
		},
	}
	for range workers {
		pconn, err := listenConfig.ListenPacket(t.Context(), "udp", listeningAddr)
		if err != nil {
			t.Fatalf("ListenPacket() error:\n%+v", err)
		}
		udpConn := pconn.(*net.UDPConn)
		listeningAddr = udpConn.LocalAddr().String()
		conns = append(conns, udpConn)
	}

	// Setup eBPF
	if err := setupReuseportEBPF(fds); err != nil {
		t.Fatalf("setupReuseportEBPF() error:\n%+v", err)
	}
	defer cleanupReuseportEBPF()

	// Start each worker
	var wg sync.WaitGroup
	receivedPackets := make([]int, workers)
	for worker := range workers {
		conn := conns[worker]
		packets := &receivedPackets[worker]
		wg.Go(func() {
			payload := make([]byte, 9000)
			for {
				if _, err := conn.Read(payload); err != nil {
					if errors.Is(err, net.ErrClosed) {
						return
					}
					t.Logf("Read() error:\n%+v", err)
				}
				*packets++
			}
		})
	}

	// Connect and send many packets
	sentPackets := 1000
	conn, err := net.Dial("udp", listeningAddr)
	if err != nil {
		t.Fatalf("Dial() error:\n%+v", err)
	}
	defer conn.Close()
	for range sentPackets {
		if _, err := conn.Write([]byte("hello world!")); err != nil {
			t.Fatalf("Write() error:\n%+v", err)
		}
	}

	// Close all listening sockets
	time.Sleep(10 * time.Millisecond)
	for worker := range workers {
		conns[worker].Close()
	}
	wg.Wait()

	// Check the number of packets received and their repartition
	got := 0
	for worker := range workers {
		received := receivedPackets[worker]
		if received < sentPackets/workers*7/10 || received > sentPackets/workers*13/10 {
			t.Errorf("receivedPackets[%d] = %d but expected ~ %d", worker, received, sentPackets/worker)
		} else {
			t.Logf("receivedPackets[%d] = %d", worker, received)
		}
		got += received
	}
	if got != sentPackets {
		t.Errorf("receivedPackets = %d but expected %d", got, sentPackets)
	}
}
