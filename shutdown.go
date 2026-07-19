package sable

// shutdown.go — R6c graceful shutdown. Tearing down the fused runtime has an
// ordering hazard: the dispatcher goroutine dereferences the global `rt` in
// drainCompletions, so freeing `rt` while it runs is a use-after-free. Shutdown
// sequences teardown so the dispatcher is stopped BEFORE the box is freed.

// #include "sable.h"
import "C"

// Shutdown gracefully tears down the global runtime: it aborts in-flight tasks
// and joins the executor thread, stops the dispatcher goroutine, then frees the
// runtime (closing Rust's eventfd). Safe to call once; idempotent-ish (a second
// call is a no-op once rt is nil). After Shutdown the runtime is unusable until
// sableInit() runs again.
func Shutdown() {
	if rt == nil {
		return
	}
	// 1. Abort in-flight tasks + join the executor. `rt` stays valid so the
	//    dispatcher can still drain any final (cancellation) completions.
	C.sable_runtime_shutdown(rt)
	// 2. Stop the dispatcher: closing our dup of the doorbell unblocks its
	//    parked Read with ErrClosed; it drains once more, then exits.
	if completionFile != nil {
		completionFile.Close()
	}
	if dispatcherDone != nil {
		<-dispatcherDone
	}
	// 3. Nothing references `rt` now — free it (drops the box, closes the
	//    eventfd Rust owns; Go already closed its dup above).
	C.sable_runtime_free(rt)
	rt = nil
}

// spawnAndFreeRuntime creates a throwaway runtime, spawns nTasks tasks that are
// still in flight, and frees it immediately — exercising teardown WITH work in
// flight (executor joined, in-flight tasks dropped, eventfd closed). Used by the
// leak tests; kept in a non-test file because _test.go files cannot import "C".
func spawnAndFreeRuntime(nTasks int) {
	r := C.sable_runtime_new()
	if r == nil {
		panic("sable_runtime_new failed")
	}
	for i := 0; i < nTasks; i++ {
		// kind 0 = sleep 1ms then return 42, so the tasks are still in flight
		// when we free (fire-and-forget: no awaiter, token is arbitrary).
		C.sable_spawn_await(r, 0, 0, C.uint64_t(i))
	}
	C.sable_runtime_free(r)
}
