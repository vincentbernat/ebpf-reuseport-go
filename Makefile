.PHONY: test
test: reuseport_bpfel.o reuseport_bpfeb.o
	go test -v ./...
reuseport_bpfel.o reuseport_bpfeb.o: reuseport_%.o: reuseport_kern.c vmlinux.h
	clang -O2 -g -Wall -target $* -c $< -o $@
