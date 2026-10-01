package main

import "encoding/binary"

// Guest-side Extism ABI for Windshift plugins.
//
// Windshift embeds github.com/extism/go-sdk on wazero. Two import surfaces
// exist for a guest module:
//
//   - extism:host/env  — the standard Extism runtime ABI (alloc/free, input,
//     output, log). Mirror of github.com/extism/go-pdk internal/memory, which
//     is an internal package and cannot be imported directly.
//   - extism:host/user — Windshift's custom host functions (log, http_fetch,
//     kv_get/kv_set/kv_delete, ...) with a single I64 pointer ABI: the guest
//     passes the offset of a JSON payload in linear memory and the host
//     returns the offset of a JSON response (0 = host-side failure).
//
// Only the imports the plugin actually calls are declared.

type extismPointer uint64

// --- extism:host/env ---

//go:wasmimport extism:host/env alloc
func extismAlloc(length uint64) extismPointer

//go:wasmimport extism:host/env free
func extismFree(ptr extismPointer)

//go:wasmimport extism:host/env store_u8
func extismStoreU8(ptr extismPointer, value uint32)

//go:wasmimport extism:host/env load_u8
func extismLoadU8(ptr extismPointer) uint32

//go:wasmimport extism:host/env length
func extismLength(ptr extismPointer) uint64

//go:wasmimport extism:host/env input_length
func extismInputLength() uint64

//go:wasmimport extism:host/env input_load_u64
func extismInputLoadU64(ptr extismPointer) uint64

//go:wasmimport extism:host/env input_load_u8
func extismInputLoadU8(ptr extismPointer) uint32

//go:wasmimport extism:host/env output_set
func extismOutputSet(ptr extismPointer, length uint64)

//go:wasmimport extism:host/env log_info
func extismLogInfo(ptr extismPointer)

// --- memory helpers ---

// allocBytes copies b into a host-allocated block and returns its offset.
func allocBytes(b []byte) extismPointer {
	if len(b) == 0 {
		return 0
	}
	ptr := extismAlloc(uint64(len(b)))
	for i, v := range b {
		extismStoreU8(ptr+extismPointer(i), uint32(v))
	}
	return ptr
}

// readBytes copies the host-tracked block at ptr back into a fresh slice.
func readBytes(ptr extismPointer) []byte {
	n := extismLength(ptr)
	if n == 0 {
		return nil
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(extismLoadU8(ptr+extismPointer(i)))
	}
	return b
}

func freeBytes(ptr extismPointer) {
	if ptr != 0 {
		extismFree(ptr)
	}
}

func logInfo(msg string) {
	ptr := allocBytes([]byte(msg))
	defer freeBytes(ptr)
	extismLogInfo(ptr)
}

// inputBytes reads the input payload the host set for this call.
// Mirrors extism/go-pdk loadInput: 8-byte chunks, then byte-wise remainder.
func inputBytes() []byte {
	n := extismInputLength()
	b := make([]byte, n)
	chunks := n >> 3
	for i := uint64(0); i < chunks; i++ {
		v := extismInputLoadU64(extismPointer(i << 3))
		binary.LittleEndian.PutUint64(b[i<<3:(i+1)<<3], v)
	}
	for i := chunks << 3; i < n; i++ {
		b[i] = byte(extismInputLoadU8(extismPointer(i)))
	}
	return b
}

// outputBytes hands b to the host as this call's output. The host copies the
// block, so it can be freed immediately.
func outputBytes(b []byte) {
	ptr := allocBytes(b)
	defer freeBytes(ptr)
	extismOutputSet(ptr, uint64(len(b)))
}

// --- Windshift host functions (extism:host/user) ---

//go:wasmimport extism:host/user kv_get
func hostKVGet(req extismPointer) extismPointer

//go:wasmimport extism:host/user kv_set
func hostKVSet(req extismPointer) extismPointer

// callKV invokes a single-payload Windshift host function with req as the
// JSON request and returns the JSON response (nil on host-side failure).
func callKV(fn func(extismPointer) extismPointer, req []byte) []byte {
	reqPtr := allocBytes(req)
	defer freeBytes(reqPtr)
	respPtr := fn(reqPtr)
	if respPtr == 0 {
		return nil
	}
	defer freeBytes(respPtr)
	return readBytes(respPtr)
}
