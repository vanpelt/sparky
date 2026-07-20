# sluice — build the eBPF object and the Go binary.
#
# The compiled BPF object (internal/meter/sluice_bpfel.o) is committed so the
# tool builds with a plain `go build` and no clang on the target. Regenerate it
# with `make bpf` after editing internal/meter/bpf/sluice.c. The bytecode is
# arch-independent across little-endian targets, so one object serves both
# amd64 and the DGX Spark's arm64.

CLANG    ?= clang
ARCH     := $(shell uname -m)
# UAPI asm/types.h lives under the multiarch include dir.
CFLAGS   := -O2 -g -target bpf -Wall -Werror -I/usr/include/$(ARCH)-linux-gnu

BPF_SRC  := internal/meter/bpf/sluice.c
BPF_OBJ  := internal/meter/sluice_bpfel.o

.PHONY: all build bpf test check clean

all: bpf build

bpf: $(BPF_OBJ)

$(BPF_OBJ): $(BPF_SRC)
	$(CLANG) $(CFLAGS) -c $< -o $@

build:
	go build -o bin/sluice ./cmd/sluice

test:
	go test ./...

# vet + full build for CI.
check:
	go vet ./...
	go build ./...

clean:
	rm -rf bin
