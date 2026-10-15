package http

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func jsonDoc(size int) []byte {
	var b strings.Builder
	b.WriteString(`{"status":"ok","items":[`)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"item-%d","price":%d.5,"tags":["a","b"]}`, 1790000000000000000+i, i, i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// BenchmarkDecodeJSON is what a json_path validator pays on every run.
func BenchmarkDecodeJSON(b *testing.B) {
	for _, size := range []int{1 << 10, 64 << 10} {
		doc := jsonDoc(size)
		b.Run(fmt.Sprintf("%dKiB/DecodeJSON", size>>10), func(b *testing.B) {
			b.SetBytes(int64(len(doc)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := DecodeJSON(doc); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("%dKiB/Unmarshal", size>>10), func(b *testing.B) {
			b.SetBytes(int64(len(doc)))
			b.ReportAllocs()
			for b.Loop() {
				var v any
				if err := json.Unmarshal(doc, &v); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
