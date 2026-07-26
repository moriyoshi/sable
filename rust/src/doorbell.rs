//! Platform-abstracted completion doorbell (R6d).
//!
//! The completion doorbell is a pure *wakeup signal*: the actual `(token,
//! result)` payload travels through `Inner.completed` (a VecDeque); the doorbell
//! only tells the Go dispatcher "the queue is non-empty, come drain it". Because
//! it carries no value, it can be any handle Go can read:
//!
//! * **Linux** → `eventfd` (one fd, counter-coalescing) — the default.
//! * **other Unix** (macOS/BSD) → a **self-pipe** (read + write fd). Go's netpoll
//!   there is kqueue, which waits on pipe fds fine.
//! * **Windows** → an anonymous **pipe** (`CreatePipe`). Go wraps the read HANDLE
//!   with `os.NewFile` and does a blocking read on it (the zero-linkname portable
//!   build has no netpoller access anyway), so no IOCP association is needed.
//!
//! The Go side is doorbell-agnostic already: `dispatcher` reads up to 8 bytes
//! then drains the logical queue to empty, so it doesn't care whether the byte
//! came from an eventfd counter or a pipe. Set `SABLE_PIPE_DOORBELL=1` to force
//! the self-pipe path on Linux (used to exercise the macOS primitive under test
//! on a Linux box).

use std::os::raw::c_int;

// ---------------------------------------------------------------------------
// Unix (Linux eventfd / other-Unix self-pipe). Unchanged from the fd-based
// original; the fast build's netpoller parks on this fd, the portable build
// blocking-reads it.
// ---------------------------------------------------------------------------

#[cfg(unix)]
use std::os::unix::io::RawFd;

#[cfg(unix)]
pub(crate) struct Doorbell {
    read_fd: RawFd,  // Go polls (a dup of) this
    write_fd: RawFd, // Rust writes this to wake Go; == read_fd for eventfd
    eventfd: bool,   // eventfd wants 8-byte writes; a pipe takes a single byte
}

#[cfg(unix)]
impl Doorbell {
    pub(crate) fn new() -> Doorbell {
        #[cfg(target_os = "linux")]
        {
            if std::env::var_os("SABLE_PIPE_DOORBELL").is_none() {
                let fd = unsafe { libc::eventfd(0, libc::EFD_NONBLOCK | libc::EFD_CLOEXEC) };
                assert!(fd >= 0, "eventfd: {}", std::io::Error::last_os_error());
                return Doorbell {
                    read_fd: fd,
                    write_fd: fd,
                    eventfd: true,
                };
            }
        }
        Self::self_pipe()
    }

    /// A nonblocking, close-on-exec self-pipe. Portable across all Unix (uses
    /// pipe(2) + fcntl rather than the Linux-only pipe2).
    fn self_pipe() -> Doorbell {
        let mut fds = [0 as c_int; 2];
        let r = unsafe { libc::pipe(fds.as_mut_ptr()) };
        assert!(r == 0, "pipe: {}", std::io::Error::last_os_error());
        for &fd in &fds {
            set_nonblock_cloexec(fd);
        }
        Doorbell {
            read_fd: fds[0],
            write_fd: fds[1],
            eventfd: false,
        }
    }

    /// The fd Go should poll (it dups this).
    pub(crate) fn read_fd(&self) -> c_int {
        self.read_fd
    }

    /// Wake the Go dispatcher. Coalescing-safe: a full pipe (or eventfd) just
    /// drops the extra token — Go drains the whole logical queue on any wake, so
    /// one pending byte suffices.
    pub(crate) fn ring(&self) {
        if self.eventfd {
            let one = 1u64.to_ne_bytes();
            let n = unsafe { libc::write(self.write_fd, one.as_ptr() as *const libc::c_void, 8) };
            debug_assert!(n == 8 || n < 0, "unexpected eventfd write {n}");
            let _ = n;
        } else {
            let byte = [1u8];
            let n = unsafe { libc::write(self.write_fd, byte.as_ptr() as *const libc::c_void, 1) };
            // EAGAIN on a full pipe is fine (coalescing); any other short write
            // is harmless — Go drains by logical queue, not by byte count.
            let _ = n;
        }
    }

    pub(crate) fn close(&self) {
        if self.read_fd >= 0 {
            unsafe { libc::close(self.read_fd) };
        }
        if self.write_fd >= 0 && self.write_fd != self.read_fd {
            unsafe { libc::close(self.write_fd) };
        }
    }
}

#[cfg(unix)]
fn set_nonblock_cloexec(fd: RawFd) {
    unsafe {
        let fl = libc::fcntl(fd, libc::F_GETFL);
        libc::fcntl(fd, libc::F_SETFL, fl | libc::O_NONBLOCK);
        let fd_fl = libc::fcntl(fd, libc::F_GETFD);
        libc::fcntl(fd, libc::F_SETFD, fd_fl | libc::FD_CLOEXEC);
    }
}

// ---------------------------------------------------------------------------
// Windows (anonymous pipe). The portable build's Go dispatcher blocking-reads
// the read HANDLE, so the doorbell only needs to be a readable/writable pipe —
// no IOCP association. Declares the tiny Win32 surface directly (no windows-sys
// dependency).
// ---------------------------------------------------------------------------

#[cfg(windows)]
use std::os::raw::c_void;

#[cfg(windows)]
type Handle = *mut c_void;
#[cfg(windows)]
type Bool = i32;
#[cfg(windows)]
type Dword = u32;

#[cfg(windows)]
const PIPE_NOWAIT: Dword = 0x0000_0001;

#[cfg(windows)]
#[link(name = "kernel32")]
extern "system" {
    fn CreatePipe(read: *mut Handle, write: *mut Handle, sa: *mut c_void, size: Dword) -> Bool;
    fn WriteFile(h: Handle, buf: *const c_void, n: Dword, written: *mut Dword, ovl: *mut c_void) -> Bool;
    fn CloseHandle(h: Handle) -> Bool;
    fn SetNamedPipeHandleState(h: Handle, mode: *const Dword, max: *const Dword, timeout: *const Dword) -> Bool;
}

#[cfg(windows)]
pub(crate) struct Doorbell {
    read: Handle,  // Go duplicates and blocking-reads this
    write: Handle, // Rust writes this to wake Go
}

// The runtime shares the doorbell across the executor thread and Go's dispatcher;
// the handles are plain owned values with no interior mutability.
#[cfg(windows)]
unsafe impl Send for Doorbell {}
#[cfg(windows)]
unsafe impl Sync for Doorbell {}

#[cfg(windows)]
impl Doorbell {
    pub(crate) fn new() -> Doorbell {
        let mut read: Handle = std::ptr::null_mut();
        let mut write: Handle = std::ptr::null_mut();
        let ok = unsafe { CreatePipe(&mut read, &mut write, std::ptr::null_mut(), 0) };
        assert!(ok != 0, "CreatePipe: {}", std::io::Error::last_os_error());
        // Nonblocking WRITE end so ring() never blocks the executor thread when the
        // pipe buffer fills — a full buffer already carries a pending wake, and Go
        // drains the whole logical queue on any read. The READ end stays blocking
        // so Go's dispatcher parks in Read until a byte lands.
        let mode = PIPE_NOWAIT;
        let _ = unsafe { SetNamedPipeHandleState(write, &mode, std::ptr::null(), std::ptr::null()) };
        Doorbell { read, write }
    }

    /// The read HANDLE Go should wrap (it duplicates this). Win32 handles are
    /// 32-bit-significant for interop, so narrowing to c_int is lossless; Go
    /// widens it back to a uintptr HANDLE.
    pub(crate) fn read_fd(&self) -> c_int {
        self.read as usize as c_int
    }

    /// Wake the Go dispatcher. Coalescing-safe: a full (nonblocking) pipe just
    /// drops the extra byte — Go drains the whole logical queue on any wake.
    pub(crate) fn ring(&self) {
        let byte = [1u8];
        let mut written: Dword = 0;
        let _ = unsafe {
            WriteFile(self.write, byte.as_ptr() as *const c_void, 1, &mut written, std::ptr::null_mut())
        };
    }

    pub(crate) fn close(&self) {
        unsafe {
            CloseHandle(self.read);
            CloseHandle(self.write);
        }
    }
}
