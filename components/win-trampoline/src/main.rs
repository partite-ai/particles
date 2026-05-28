//! Native Windows launcher stub for `particle link` (no_std).
//!
//! `particle link` writes a copy of this compiled exe with a trailer
//! appended that describes what to run:
//!
//! ```text
//! [ trampoline.exe bytes ][ payload ][ u32 payloadLen ][ b"PRTCLNK1" ]
//! ```
//!
//! `payload` is a little-endian length-prefixed UTF-8 argv:
//!
//! ```text
//! u32 argc
//! argc × ( u32 byteLen, <byteLen> UTF-8 bytes )
//! ```
//!
//! `argv[0]` is the particle binary to launch; the rest is the fixed
//! `run [--db <db>] <target>` prefix. At run time we read our own
//! file, parse the trailer, append the *verbatim* tail of our own
//! command line (everything after our argv[0], already quoted by the
//! caller), and spawn the particle binary — in a kill-on-close job
//! object so the child dies with us, Ctrl+C left to the child,
//! propagating its exit code. Keep this format in lockstep with the
//! Go encoder in cmd/particle/link_windows.go.
//!
//! Adapted from njsmith/posy and astral-sh/uv's Windows trampolines.
//! Unlike those (which target MSVC and drop the CRT via /ENTRY), we
//! build for *-pc-windows-gnullvm and keep the mingw CRT, which gives
//! us the entry point and the mem intrinsics for free; we stay no_std
//! to avoid pulling in the Rust standard library.

#![no_std]
#![no_main]

extern crate alloc;

use alloc::string::String;
use alloc::vec::Vec;
use core::ffi::c_void;
use core::panic::PanicInfo;
use core::ptr::{null, null_mut};

use windows_sys::Win32::Foundation::{CloseHandle, GetLastError};
use windows_sys::Win32::Storage::FileSystem::{CreateFileW, GetFileSizeEx, ReadFile};
use windows_sys::Win32::System::Console::SetConsoleCtrlHandler;
use windows_sys::Win32::System::Environment::GetCommandLineW;
use windows_sys::Win32::System::JobObjects::{
    AssignProcessToJobObject, CreateJobObjectW, SetInformationJobObject,
    JOBOBJECT_EXTENDED_LIMIT_INFORMATION,
};
use windows_sys::Win32::System::LibraryLoader::GetModuleFileNameW;
use windows_sys::Win32::System::Memory::{
    GetProcessHeap, HeapAlloc, HeapFree, HeapReAlloc, HEAP_ZERO_MEMORY,
};
use windows_sys::Win32::System::Threading::{
    CreateProcessW, ExitProcess, GetExitCodeProcess, ResumeThread, WaitForSingleObject,
    PROCESS_INFORMATION, STARTUPINFOW,
};

// ABI-stable constants, defined locally so we don't depend on exact
// windows-sys constant names/locations across versions.
const GENERIC_READ: u32 = 0x8000_0000;
const FILE_SHARE_READ: u32 = 0x0000_0001;
const OPEN_EXISTING: u32 = 3;
const FILE_ATTRIBUTE_NORMAL: u32 = 0x0000_0080;
const CREATE_SUSPENDED: u32 = 0x0000_0004;
const INFINITE: u32 = 0xFFFF_FFFF;
const JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: u32 = 0x0000_2000;
const JOB_OBJECT_EXTENDED_LIMIT_INFORMATION_CLASS: i32 = 9;

const MAGIC: &[u8; 8] = b"PRTCLNK1";

// ---- no_std plumbing --------------------------------------------------------

struct WinHeap;

unsafe impl alloc::alloc::GlobalAlloc for WinHeap {
    unsafe fn alloc(&self, layout: alloc::alloc::Layout) -> *mut u8 {
        HeapAlloc(GetProcessHeap(), 0, layout.size()) as *mut u8
    }
    unsafe fn alloc_zeroed(&self, layout: alloc::alloc::Layout) -> *mut u8 {
        HeapAlloc(GetProcessHeap(), HEAP_ZERO_MEMORY, layout.size()) as *mut u8
    }
    unsafe fn dealloc(&self, ptr: *mut u8, _layout: alloc::alloc::Layout) {
        HeapFree(GetProcessHeap(), 0, ptr as *const c_void);
    }
    unsafe fn realloc(&self, ptr: *mut u8, _layout: alloc::alloc::Layout, new: usize) -> *mut u8 {
        HeapReAlloc(GetProcessHeap(), 0, ptr as *const c_void, new) as *mut u8
    }
}

#[global_allocator]
static ALLOC: WinHeap = WinHeap;

#[panic_handler]
fn panic(_info: &PanicInfo) -> ! {
    unsafe { ExitProcess(101) }
}

// The compiler emits a reference to this when any floating point is
// used; defining it avoids a link error. Harmless if unused.
#[no_mangle]
#[used]
static _fltused: i32 = 0;

fn die(msg: &str) -> ! {
    // Best-effort stderr write via the CRT's fputs is avoided (no_std);
    // we just emit the bytes through WriteFile on the std error handle.
    unsafe {
        use windows_sys::Win32::Storage::FileSystem::WriteFile;
        use windows_sys::Win32::System::Console::GetStdHandle;
        const STD_ERROR_HANDLE: u32 = 0xFFFF_FFF4; // (DWORD)-12
        let h = GetStdHandle(STD_ERROR_HANDLE);
        let prefix = b"particle link launcher: ";
        let mut written = 0u32;
        WriteFile(
            h,
            prefix.as_ptr(),
            prefix.len() as u32,
            &mut written,
            null_mut(),
        );
        WriteFile(h, msg.as_ptr(), msg.len() as u32, &mut written, null_mut());
        let nl = b"\n";
        WriteFile(h, nl.as_ptr(), 1, &mut written, null_mut());
        ExitProcess(1)
    }
}

// ---- trailer parsing --------------------------------------------------------

fn parse_trailer(data: &[u8]) -> Option<Vec<String>> {
    if data.len() < MAGIC.len() + 4 {
        return None;
    }
    let (head, magic) = data.split_at(data.len() - MAGIC.len());
    if magic != MAGIC {
        return None;
    }
    let (head, len_bytes) = head.split_at(head.len() - 4);
    let payload_len = u32::from_le_bytes(len_bytes.try_into().ok()?) as usize;
    if payload_len > head.len() {
        return None;
    }
    let mut p = &head[head.len() - payload_len..];

    let argc = read_u32(&mut p)? as usize;
    let mut args = Vec::with_capacity(argc);
    for _ in 0..argc {
        let n = read_u32(&mut p)? as usize;
        if n > p.len() {
            return None;
        }
        let (s, rest) = p.split_at(n);
        args.push(String::from_utf8(s.to_vec()).ok()?);
        p = rest;
    }
    Some(args)
}

fn read_u32(p: &mut &[u8]) -> Option<u32> {
    if p.len() < 4 {
        return None;
    }
    let (n, rest) = p.split_at(4);
    *p = rest;
    Some(u32::from_le_bytes(n.try_into().ok()?))
}

// ---- command-line assembly --------------------------------------------------

/// Append `arg` as one command-line token, quoting per the
/// CommandLineToArgvW rules so the child reconstructs it exactly.
fn append_quoted(arg: &[u16], out: &mut Vec<u16>) {
    const SPACE: u16 = b' ' as u16;
    const TAB: u16 = b'\t' as u16;
    const QUOTE: u16 = b'"' as u16;
    const BACKSLASH: u16 = b'\\' as u16;

    let needs = arg.is_empty() || arg.iter().any(|&c| c == SPACE || c == TAB || c == QUOTE);
    if !needs {
        out.extend_from_slice(arg);
        return;
    }
    out.push(QUOTE);
    let mut i = 0;
    while i < arg.len() {
        let mut bs = 0;
        while i < arg.len() && arg[i] == BACKSLASH {
            bs += 1;
            i += 1;
        }
        if i == arg.len() {
            for _ in 0..bs * 2 {
                out.push(BACKSLASH);
            }
        } else if arg[i] == QUOTE {
            for _ in 0..bs * 2 + 1 {
                out.push(BACKSLASH);
            }
            out.push(QUOTE);
            i += 1;
        } else {
            for _ in 0..bs {
                out.push(BACKSLASH);
            }
            out.push(arg[i]);
            i += 1;
        }
    }
    out.push(QUOTE);
}

/// Borrow the process command line as a UTF-16 slice (no trailing NUL).
unsafe fn command_line() -> &'static [u16] {
    let p = GetCommandLineW();
    let mut len = 0usize;
    while *p.add(len) != 0 {
        len += 1;
    }
    core::slice::from_raw_parts(p, len)
}

/// Return the slice of `cmd` after argv[0] and any following spaces —
/// i.e. the caller's arguments, already correctly quoted. We append
/// these verbatim so we never re-quote user input.
fn args_tail(cmd: &[u16]) -> &[u16] {
    const SPACE: u16 = b' ' as u16;
    const TAB: u16 = b'\t' as u16;
    const QUOTE: u16 = b'"' as u16;

    let mut i = 0;
    if cmd.first() == Some(&QUOTE) {
        i = 1;
        while i < cmd.len() && cmd[i] != QUOTE {
            i += 1;
        }
        if i < cmd.len() {
            i += 1; // consume closing quote
        }
    } else {
        while i < cmd.len() && cmd[i] != SPACE && cmd[i] != TAB {
            i += 1;
        }
    }
    while i < cmd.len() && (cmd[i] == SPACE || cmd[i] == TAB) {
        i += 1;
    }
    &cmd[i..]
}

fn wide_nul(s: &str) -> Vec<u16> {
    let mut v: Vec<u16> = s.encode_utf16().collect();
    v.push(0);
    v
}

// ---- read our own exe -------------------------------------------------------

unsafe fn read_self() -> Vec<u8> {
    let mut path = Vec::<u16>::with_capacity(0x8000);
    let n = GetModuleFileNameW(null_mut(), path.as_mut_ptr(), 0x8000);
    if n == 0 {
        die("GetModuleFileNameW failed");
    }
    path.set_len(n as usize);
    path.push(0);

    let h = CreateFileW(
        path.as_ptr(),
        GENERIC_READ,
        FILE_SHARE_READ,
        null(),
        OPEN_EXISTING,
        FILE_ATTRIBUTE_NORMAL,
        null_mut(),
    );
    if h as isize == -1 {
        die("cannot open launcher executable");
    }
    let mut size: i64 = 0;
    if GetFileSizeEx(h, &mut size) == 0 || size <= 0 {
        die("cannot size launcher executable");
    }
    let size = size as usize;
    let mut buf = Vec::<u8>::with_capacity(size);
    let mut total = 0usize;
    while total < size {
        let mut read = 0u32;
        let ok = ReadFile(
            h,
            buf.as_mut_ptr().add(total),
            (size - total) as u32,
            &mut read,
            null_mut(),
        );
        if ok == 0 || read == 0 {
            break;
        }
        total += read as usize;
    }
    buf.set_len(total);
    CloseHandle(h);
    buf
}

// ---- entry ------------------------------------------------------------------

unsafe extern "system" fn ignore_ctrl(_ctrl_type: u32) -> i32 {
    1 // handled — swallow it in the launcher; the child gets its own copy
}

#[no_mangle]
pub extern "C" fn main(_argc: i32, _argv: *mut *mut u8) -> i32 {
    unsafe {
        let data = read_self();
        let prefix = match parse_trailer(&data) {
            Some(p) if !p.is_empty() => p,
            _ => die("this launcher has no particle-link trailer (corrupt or not produced by `particle link`)"),
        };

        // Build the child command line: quoted prefix args, then the
        // caller's argument tail appended verbatim.
        let mut cmdline: Vec<u16> = Vec::new();
        for (i, a) in prefix.iter().enumerate() {
            if i > 0 {
                cmdline.push(b' ' as u16);
            }
            let w: Vec<u16> = a.encode_utf16().collect();
            append_quoted(&w, &mut cmdline);
        }
        let tail = args_tail(command_line());
        if !tail.is_empty() {
            cmdline.push(b' ' as u16);
            cmdline.extend_from_slice(tail);
        }
        cmdline.push(0);

        let app = wide_nul(&prefix[0]);

        // Let the child own Ctrl+C: returning TRUE from a handler we
        // install makes this process ignore it while the console still
        // delivers it to the child.
        SetConsoleCtrlHandler(Some(ignore_ctrl), 1);

        // Kill-on-close job: if the launcher is terminated, the child
        // is torn down too — no orphaned particle processes.
        let job = CreateJobObjectW(null(), null());
        if !job.is_null() {
            let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = core::mem::zeroed();
            info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
            SetInformationJobObject(
                job,
                JOB_OBJECT_EXTENDED_LIMIT_INFORMATION_CLASS,
                &info as *const _ as *const c_void,
                core::mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
            );
        }

        let mut si: STARTUPINFOW = core::mem::zeroed();
        si.cb = core::mem::size_of::<STARTUPINFOW>() as u32;
        let mut pi: PROCESS_INFORMATION = core::mem::zeroed();

        // Start suspended so we can place the child in the job before
        // it runs, then resume.
        let ok = CreateProcessW(
            app.as_ptr(),
            cmdline.as_mut_ptr(),
            null(),
            null(),
            1, // inherit handles (stdio)
            CREATE_SUSPENDED,
            null(),
            null(),
            &si,
            &mut pi,
        );
        if ok == 0 {
            let _ = GetLastError();
            die("failed to launch the particle binary");
        }
        if !job.is_null() {
            AssignProcessToJobObject(job, pi.hProcess);
        }
        ResumeThread(pi.hThread);
        CloseHandle(pi.hThread);

        WaitForSingleObject(pi.hProcess, INFINITE);
        let mut code: u32 = 0;
        GetExitCodeProcess(pi.hProcess, &mut code);
        ExitProcess(code)
    }
}
