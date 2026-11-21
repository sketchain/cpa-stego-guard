package stego

import (
	"strings"
	"testing"
)

// largeBody builds a Claude Code shaped body of roughly size bytes: the marked
// first user message followed by many conversation turns.
func largeBody(b *testing.B, size int) []byte {
	b.Helper()
	f := fixture{dateLine: "Today’s date is 2026/10/09.", secondUser: strings.Repeat("tool output line\n", size/17)}
	return f.build(&testing.T{})
}

func BenchmarkScan2MB(b *testing.B) {
	body := largeBody(b, 2<<20)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := Scan(body); len(r.DateLines) != 1 {
			b.Fatal("marker not found")
		}
	}
}

func BenchmarkScanNonClaude2MB(b *testing.B) {
	body := []byte(`{"contents":[{"parts":[{"text":"` + strings.Repeat("x", 2<<20) + `"}]}]}`)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Scan(body)
	}
}
