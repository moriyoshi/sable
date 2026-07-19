//! Platform-abstracted completion doorbell (R6d).
//!
//! The completion doorbell is a pure *wakeup signal*: the actual `(token,
//! result)` payload travels through `Inner.completed` (a VecDeque); the doorbell
//! only tells the Go dispatcher "the queue is non-empty, come drain it". Because
//! it carries no value, it can be any fd Go's netpoller can wait on:
//!
//! * **Linux** → `eventfd` (one fd, counter-coalescing) — the default.
//! * **other Unix** (macOS/BSD) → a **self-pipe** (read + write fd). Go's netpoll
//!   there is kqueue, which waits on pipe fds fine.
//!
//! The Go side is doorbell-agnostic already: `dispatcher` reads up to 8 bytes
//! then drains the logical queue to empty, so it doesn't care whether the byte
//! came from an eventfd counter or a pipe. Set `SABLE_PIPE_DOORBELL=1` to force
//! the self-pipe path on Linux (used to exercise the macOS primitive under test
//! on a Linux box). Windows (IOCP) needs a different handle and is a follow-on;
//! see README "Operating-system support".

use std::os::raw::c_int;
use std::os::unix::io::RawFd;

pub(crate) struct Doorbell {
    read_fd: RawFd,  // Go polls (a dup of) this
    write_fd: RawFd, // Rust writes this to wake Go; == read_fd for eventfd
    eventfd: bool,   // eventfd wants 8-byte writes; a pipe takes a single byte
}

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
    pub(crate) fn read_fd(&self) -> RawFd {
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

fn set_nonblock_cloexec(fd: RawFd) {
    unsafe {
        let fl = libc::fcntl(fd, libc::F_GETFL);
        libc::fcntl(fd, libc::F_SETFL, fl | libc::O_NONBLOCK);
        let fd_fl = libc::fcntl(fd, libc::F_GETFD);
        libc::fcntl(fd, libc::F_SETFD, fd_fl | libc::FD_CLOEXEC);
    }
}
