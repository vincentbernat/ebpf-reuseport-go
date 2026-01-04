// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package main

import (
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func setupSockets(t *testing.T, workers int, listenAddr string) (fds []uintptr, conns []*net.UDPConn) {
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
		pconn, err := listenConfig.ListenPacket(t.Context(), "udp", listenAddr)
		if err != nil {
			t.Fatalf("ListenPacket() error:\n%+v", err)
		}
		udpConn := pconn.(*net.UDPConn)
		listenAddr = udpConn.LocalAddr().String()
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
