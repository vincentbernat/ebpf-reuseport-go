// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package main

import (
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// TestUDPWorkerBalancing tests that incoming connections are load balanced
// evenly over the various workers.
func TestUDPWorkerBalancing(t *testing.T) {
	workers := 10
	fds, conns := setupSockets(t, workers, "127.0.0.1:0")
	setupEBPF(t, fds)

	// Start each worker
	var wg sync.WaitGroup
	done := make(chan bool)
	receivedPackets := make([]int, workers)
	for worker := range workers {
		conn := conns[worker]
		packets := &receivedPackets[worker]
		wg.Go(func() {
			payload := make([]byte, 9000)
			for {
				// In Go, it is difficult to do a graceful shutdown of an UDP
				// socket. The only way to unblock a Read() is either by
				// receiving a packet or with an error. Closing the socket to
				// get an error also discards the data. Therefore, a timeout is
				// used, but this delays the correct shutdown and it actively
				// pools the socket.
				conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				if _, err := conn.Read(payload); err != nil {
					if errors.Is(err, os.ErrDeadlineExceeded) {
						select {
						case <-done:
							return
						default:
							continue
						}
					}
					t.Logf("Read() error:\n%+v", err)
				}
				*packets++
			}
		})
	}

	// Connect and send many packets
	sentPackets := 1000
	conn, err := net.Dial("udp", conns[0].LocalAddr().String())
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
	close(done)
	wg.Wait()
	for worker := range workers {
		conns[worker].Close()
	}

	// Check the number of packets received and their repartition
	got := 0
	for worker := range workers {
		received := receivedPackets[worker]
		if received < sentPackets/workers*7/10 || received > sentPackets/workers*13/10 {
			t.Errorf("receivedPackets[%d] = %d but expected ~ %d", worker, received, sentPackets/workers)
		} else {
			t.Logf("receivedPackets[%d] = %d", worker, received)
		}
		got += received
	}
	if got != sentPackets {
		t.Errorf("receivedPackets = %d but expected %d", got, sentPackets)
	}
}
