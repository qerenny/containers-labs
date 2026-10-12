package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
)

const mebibyte = 1024 * 1024

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	memoryMiB := positiveIntEnv("BATCH_MEMORY_MIB", 64, logger)
	cpuWorkers := positiveIntEnv("BATCH_CPU_WORKERS", 1, logger)

	// Touch every page so the allocation becomes resident memory rather than
	// merely reserving virtual address space.
	memory := make([]byte, memoryMiB*mebibyte)
	for offset := 0; offset < len(memory); offset += os.Getpagesize() {
		memory[offset] = byte(offset)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("batch load started", "memory_mib", memoryMiB, "cpu_workers", cpuWorkers)

	var checksum atomic.Uint64
	var workers sync.WaitGroup
	workers.Add(cpuWorkers)
	for workerID := 0; workerID < cpuWorkers; workerID++ {
		go burnCPU(ctx, uint64(workerID+1), &checksum, &workers)
	}

	<-ctx.Done()
	workers.Wait()
	runtime.KeepAlive(memory)
	logger.Info("batch load stopped", "checksum", checksum.Load())
}

func burnCPU(ctx context.Context, seed uint64, checksum *atomic.Uint64, workers *sync.WaitGroup) {
	defer workers.Done()

	value := seed
	for {
		// Work in chunks so cancellation is observed without turning the hot loop
		// into a benchmark of channel operations.
		for iteration := 0; iteration < 1_000_000; iteration++ {
			value ^= value << 13
			value ^= value >> 7
			value ^= value << 17
		}
		checksum.Add(value)
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func positiveIntEnv(name string, fallback int, logger *slog.Logger) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		logger.Error("environment variable must be a positive integer", "name", name, "value", raw)
		os.Exit(2)
	}
	return value
}
