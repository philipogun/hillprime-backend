package auth

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
)

const maxBodyBytes = 1 << 20 // 1 MiB

func readAll(r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	return io.ReadAll(r.Body)
}

func restoreBody(r *http.Request, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
}

func itoa(n uint32) string {
	return fmt.Sprintf("%d", n)
}

func parseKV(s string, m, t *uint32, p *uint8) (int, error) {
	// Expected: "m=65536,t=3,p=4"
	return fmt.Sscanf(s, "m=%d,t=%d,p=%d", m, t, p)
}
