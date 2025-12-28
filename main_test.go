// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package main

import (
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func setupSockets(t *testing.T, workers int, listeningAddr string) (fds []uintptr, conns []*net.UDPConn) {
	// For each worker, setup the listening sockets with SO_REUSEPORT
	var err error
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
	return
}

func setupEBPF(t *testing.T, fds []uintptr) {
	// Load the eBPF collection.
	spec, err := loadReuseport()
	if err != nil {
		t.Fatalf("loadVariables() error:\n%+v", err)
	}
	// Set "num_sockets" global variable to the number of file descriptors we will register
	if err := spec.Variables["num_sockets"].Set(uint32(len(fds))); err != nil {
		t.Fatalf("NumSockets.Set() error:\n%+v", err)
	}
	// Load the map and the program into the kernel.
	var objs reuseportObjects
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		t.Fatalf("loadReuseportObjects() error:\n%+v", err)
	}
	t.Cleanup(func() { objs.Close() })
	// Assign the file descriptors to the socket map.
	for worker, fd := range fds {
		if err := objs.reuseportMaps.SocketMap.Put(uint32(worker), uint64(fd)); err != nil {
			t.Fatalf("SocketMap.Put() error:\n%+v", err)
		}
	}
	// Attach the eBPF program to the first socket.
	socketFD := int(fds[0])
	progFD := objs.reuseportPrograms.ReuseportBalanceProg.FD()
	if err := unix.SetsockoptInt(socketFD, unix.SOL_SOCKET, unix.SO_ATTACH_REUSEPORT_EBPF, progFD); err != nil {
		t.Fatalf("SetsockoptInt() error:\n%+v", err)
	}

}

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

// TestZeroDowntime checks another set of workers can take over without any
// downtime.
func TestZeroDowntime(t *testing.T) {
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
}
