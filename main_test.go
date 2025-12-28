// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

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

func TestUDPWorkerBalancing(t *testing.T) {
	workers := 10

	// For each worker, setup the listening sockets with SO_REUSEPORT
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
	setupEBPF(t, fds)

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
