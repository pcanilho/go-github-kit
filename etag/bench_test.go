package etag_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/pcanilho/go-github-kit/etag"
)

// BenchmarkETag_ComputeExpectedETag measures raw SHA256 throughput over the
// canonical hash domain (Authorization + Accept + body). The 304-synthesis
// hot path runs this on every cache hit, so the per-byte cost here directly
// dictates the ETag layer's CPU ceiling.
func BenchmarkETag_ComputeExpectedETag(b *testing.B) {
	headers := http.Header{
		"Authorization": {"Bearer ghs_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
		"Accept":        {"application/vnd.github.v3+json"},
	}
	for _, size := range []int{1024, 4096, 16384, 65536} {
		body := make([]byte, size)
		for i := range body {
			body[i] = 'x'
		}
		b.Run(strconv.Itoa(size)+"B", func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for range b.N {
				_ = etag.ComputeExpectedETag(headers, nil, body)
			}
		})
	}
}
