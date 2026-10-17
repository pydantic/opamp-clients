// Copyright (c) 2026 Pydantic Services Inc.
// SPDX-License-Identifier: MIT

package opamp

import (
	"bytes"
	"runtime"
	"strconv"
)

// goid is the calling goroutine's id, from the first line of its stack ("goroutine 18
// [running]:"); 0 if that can't be read. Stop uses it to know it was called on the client's own
// goroutine, from the callback: Go has no other way to tell, and Stop is called rarely.
func goid() uint64 {
	var buf [64]byte
	b := bytes.TrimPrefix(buf[:runtime.Stack(buf[:], false)], []byte("goroutine "))
	if i := bytes.IndexByte(b, ' '); i > 0 {
		if id, err := strconv.ParseUint(string(b[:i]), 10, 64); err == nil {
			return id
		}
	}
	return 0
}
