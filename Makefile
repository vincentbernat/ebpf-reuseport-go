GENERATED_BPF2GO = reuseport_bpfel.o reuseport_bpfeb.o reuseport_bpfeb.go reuseport_bpfel.go
HAS_CAP_BPF = $(shell capsh --has-a=cap_bpf 2> /dev/null && echo yes)

.PHONY: test
ifeq ($(HAS_CAP_BPF),yes)
test: $(GENERATED_BPF2GO)
	go test -timeout 5s -v ./...
else
test:
	@sudo -E capsh --keep=1 --user=$$USER --caps=cap_bpf+eip --addamb=cap_bpf -- -c "$(MAKE) test"
endif

$(GENERATED_BPF2GO) &: reuseport_kern.c vmlinux.h
	go generate ./...
