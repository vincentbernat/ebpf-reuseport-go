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

// TestGracefulRestart checks another set of workers can take over without any
// downtime.
func TestGracefulRestart(t *testing.T) {
	workers := 10
	fds1, conns1 := setupSockets(t, workers, "127.0.0.1:0")
	setupEBPF(t, fds1)

	// Start the first set of workers
	var wg1 sync.WaitGroup
	done1 := make(chan bool)
	receivedPackets1 := make([]int, workers)
	for worker := range workers {
		conn := conns1[worker]
		packets := &receivedPackets1[worker]
		wg1.Go(func() {
			payload := make([]byte, 9000)
			for {
				conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				if _, err := conn.Read(payload); err != nil {
					if errors.Is(err, os.ErrDeadlineExceeded) {
						select {
						case <-done1:
							return
						default:
							continue
						}
					}
					t.Logf("Read() error:\n%+v", err)
				}
				*packets++
				// First worker is a bit slow. This enables to test that
				// remaining packets in queue are not lost.
				if worker == 0 {
					time.Sleep(time.Millisecond / 10)
				}
			}
		})
	}

	// Connect and send many packets
	sentPackets := 0
	notSentPackets := 0
	done := make(chan bool)
	conn, err := net.Dial("udp", conns1[0].LocalAddr().String())
	if err != nil {
		t.Fatalf("Dial() error:\n%+v", err)
	}
	defer conn.Close()
	go func() {
		for {
			if _, err := conn.Write([]byte("hello world!")); err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				notSentPackets++
			} else {
				sentPackets++
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	time.Sleep(10 * time.Millisecond)

	// Start the second set of workers
	fds2, conns2 := setupSockets(t, workers, conns1[0].LocalAddr().String())
	var wg2 sync.WaitGroup
	done2 := make(chan bool)
	receivedPackets2 := make([]int, workers)
	for worker := range workers {
		conn := conns2[worker]
		packets := &receivedPackets2[worker]
		wg2.Go(func() {
			payload := make([]byte, 9000)
			for {
				conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				if _, err := conn.Read(payload); err != nil {
					if errors.Is(err, os.ErrDeadlineExceeded) {
						select {
						case <-done2:
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

	// Switch to the second set of workers and stop the first set
	setupEBPF(t, fds2)
	close(done1)
	wg1.Wait()
	for worker := range workers {
		conns1[worker].Close()
	}

	// Wait a bit and stop sending packets
	time.Sleep(10 * time.Millisecond)
	close(done)

	// Stop the second set of workers
	time.Sleep(10 * time.Millisecond)
	close(done2)
	wg2.Wait()
	for worker := range workers {
		conns2[worker].Close()
	}

	totalReceivedPackets := 0
	for worker, received := range receivedPackets1 {
		totalReceivedPackets += received
		t.Logf("receivedPackets1[%d] = %d", worker, received)
	}
	for worker, received := range receivedPackets2 {
		totalReceivedPackets += received
		t.Logf("receivedPackets2[%d] = %d", worker, received)
	}
	if notSentPackets > 0 {
		t.Errorf("notSentPackets = %d > 0", notSentPackets)
	}
	if totalReceivedPackets != sentPackets {
		t.Errorf("totalReceivedPackets (%d) != sentPackets (%d)", totalReceivedPackets, sentPackets)
	}
	t.Logf("receivedPackets = %d", totalReceivedPackets)
	t.Logf("sentPackets     = %d", sentPackets)
}
