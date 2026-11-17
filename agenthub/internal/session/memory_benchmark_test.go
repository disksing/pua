package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func memoryHistoryFixture(tb testing.TB, root string, count, payloadBytes int) string {
	tb.Helper()
	store, err := Open(root)
	if err != nil {
		tb.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Synthetic memory workload", Cwd: root})
	if err != nil {
		tb.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(root, created.ID, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		tb.Fatal(err)
	}
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	data, _ := json.Marshal(map[string]string{"text": strings.Repeat("x", payloadBytes)})
	value := created
	for index := 0; index < count; index++ {
		event := Event{ID: int64(index + 2), Time: time.Now().UTC(), SessionID: created.ID, Type: "provider.metadata", Data: data}
		if err := encoder.Encode(event); err != nil {
			tb.Fatal(err)
		}
		value.LastEventID = event.ID
		value.UpdatedAt = event.Time
	}
	if err := writer.Flush(); err != nil {
		tb.Fatal(err)
	}
	if err := file.Close(); err != nil {
		tb.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(root, created.ID, "session.json"), value); err != nil {
		tb.Fatal(err)
	}
	return created.ID
}

func BenchmarkStoreMemoryLifecycle(b *testing.B) {
	root := b.TempDir()
	sessions, events := 8, 4096
	if os.Getenv("AGENTHUB_MEMORY_SCENARIO") == "single-long" {
		sessions, events = 1, 32768
	}
	ids := make([]string, sessions)
	for index := range ids {
		ids[index] = memoryHistoryFixture(b, root, events, 4096)
	}
	store, err := Open(root)
	if err != nil {
		b.Fatal(err)
	}
	sample := func(phase string) {
		debug.FreeOSMemory()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		resident, _ := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
		b.Logf("MEM phase=%s pid=%d heap=%d heapSys=%d rssKiB=%s goroutines=%d cached=%d", phase, os.Getpid(), stats.HeapAlloc, stats.HeapSys, strings.TrimSpace(string(resident)), runtime.NumGoroutine(), store.Diagnostics().CachedEventPayloadBytes)
		if directory := os.Getenv("AGENTHUB_MEMORY_PROFILES"); directory != "" {
			file, err := os.Create(filepath.Join(directory, phase+".heap"))
			if err != nil {
				b.Fatal(err)
			}
			if err := pprof.WriteHeapProfile(file); err != nil {
				b.Fatal(err)
			}
			if err := file.Close(); err != nil {
				b.Fatal(err)
			}
		}
	}
	sample("idle")
	start := time.Now()
	for _, id := range ids {
		events, err := store.EventsAfter(id, 0, 100)
		if err != nil || len(events) != 100 {
			b.Fatalf("history: %d events, %v", len(events), err)
		}
	}
	b.Logf("LATENCY phase=read-eight elapsed=%s", time.Since(start))
	sample("read-eight")
	start = time.Now()
	for index := 0; index < 128; index++ {
		events, err := store.EventsAfter(ids[index%len(ids)], 2048, 100)
		if err != nil || len(events) != 100 {
			b.Fatalf("warm page: %d events, %v", len(events), err)
		}
	}
	b.Logf("LATENCY phase=warm-pages-128 elapsed=%s", time.Since(start))
	latencies := make([]time.Duration, 0, 128)
	start = time.Now()
	for index := 0; index < 128; index++ {
		before := time.Now()
		if _, err := store.Append(ids[index%len(ids)], "provider.metadata", "", []byte(`{"text":"next"}`)); err != nil {
			b.Fatal(err)
		}
		latencies = append(latencies, time.Since(before))
	}
	elapsed := time.Since(start)
	sort.Slice(latencies, func(left, right int) bool { return latencies[left] < latencies[right] })
	b.Logf("LATENCY phase=append-128 elapsed=%s p50=%s p95=%s opsPerSecond=%.2f", elapsed, latencies[len(latencies)/2], latencies[len(latencies)*95/100], 128/elapsed.Seconds())
	sample("append-eight")
	for _, id := range ids {
		if _, err := store.Append(id, "session.state", "", []byte(`{"state":"stopped","reason":"requested"}`)); err != nil {
			b.Fatal(err)
		}
	}
	sample("stopped-eight")
	for _, id := range ids {
		if _, err := store.Archive(id); err != nil {
			b.Fatal(err)
		}
		if events, err := store.EventsAfter(id, 3900, 100); err != nil || len(events) != 100 {
			b.Fatalf("archived history: %d events, %v", len(events), err)
		}
	}
	sample("archived-eight")
	runtime.KeepAlive(store)
	b.ReportMetric(float64(store.Diagnostics().CachedEventPayloadBytes), "cached-bytes")
	b.ReportMetric(float64(len(ids)), "sessions")
}

func BenchmarkStoreHistoryPage(b *testing.B) {
	for _, count := range []int{256, 4096} {
		b.Run(fmt.Sprintf("events-%d", count), func(b *testing.B) {
			root := b.TempDir()
			id := memoryHistoryFixture(b, root, count, 4096)
			store, err := Open(root)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if _, err := store.EventsAfter(id, int64(count/2), 100); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
