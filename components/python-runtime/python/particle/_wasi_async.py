"""
Particle's asyncio integration over wasi:io/poll.

What we replace
---------------
Two things, both tightly scoped:

  1. The selector: `HybridSelector` composes a lazy stdlib selector
     (for real fds — works on wasi:p2 because wasi-libc's `poll(2)`
     translates to wasi:io/poll under the hood) with a wasi-pollable
     tracker (for handles minted by `_runtime_host._http_pollable`
     and any future async primitives).

  2. `BaseSelectorEventLoop._make_self_pipe` and its
     `_write_to_self` / `_close_self_pipe` siblings — they call
     `socket.socketpair()`, which doesn't exist on wasi. Stripping
     them to no-ops is unavoidable; wasi is single-threaded, so
     cross-thread wakeup is moot.

What we don't replace
---------------------
Almost everything else. `asyncio.SelectorEventLoop`'s `_run_once`,
the task scheduler, timers, futures, `Queue` / `Event` / `Lock`,
`gather`, `wait_for`, `sleep` — all unchanged. The minimal subclass
exists only to swap in `HybridSelector` and no-op the self-pipe.

Routing wasi pollables vs. real fds
-----------------------------------
Pollable handles and real fds are both ints, and asyncio's
`loop.add_reader(fd, callback)` doesn't distinguish them. The
selector uses an explicit side-channel: `WasiHttpFuture` calls
`selector.announce_wasi(handle)` before `loop.add_reader(handle,
...)`, so `register()` knows to route it to the wasi branch. Real
fds, which arrive at `register()` without an announcement, flow to
the stdlib selector.

Mixed-wait latency
------------------
When one `await` is sleeping on a wasi pollable and a concurrent
`await` is sleeping on a real fd, we can't compose the two waits
into a single host blocking call — wasi:io/poll and wasi-libc's
poll(2) don't share a primitive at the Python level. So
`HybridSelector.select(timeout)` falls back to slicing: poll the
wasi pollables with a short timeout, then non-blocking-poll the
stdlib selector, repeat until something fires or the overall
timeout expires. The slice size sets the worst-case wake-up
latency the mixed-wait branch adds — small enough to not be
noticeable for normal interactive code, large enough that the
spin doesn't burn CPU.
"""

import asyncio
import selectors
import time
from collections.abc import Mapping
from selectors import EVENT_READ, EVENT_WRITE, SelectorKey

import _runtime_host

__all__ = [
    "HybridSelector",
    "WasiEventLoop",
    "WasiEventLoopPolicy",
    "WasiHttpFuture",
    "install_policy",
]


# -- selector ---------------------------------------------------------------

class _HybridMapping(Mapping):
    """Union view of wasi + stdlib selector entries, the shape
    `BaseSelector.get_map()` promises."""

    def __init__(self, hybrid: "HybridSelector"):
        self._hybrid = hybrid

    def __getitem__(self, fd):
        if fd in self._hybrid._wasi_keys:
            return self._hybrid._wasi_keys[fd]
        stdlib = self._hybrid._stdlib
        if stdlib is not None:
            return stdlib.get_map()[fd]
        raise KeyError(fd)

    def __iter__(self):
        seen = set()
        for fd in self._hybrid._wasi_keys:
            seen.add(fd)
            yield fd
        if self._hybrid._stdlib is not None:
            for fd in self._hybrid._stdlib.get_map():
                if fd not in seen:
                    yield fd

    def __len__(self):
        n = len(self._hybrid._wasi_keys)
        if self._hybrid._stdlib is not None:
            n += len(self._hybrid._stdlib.get_map())
        return n


class HybridSelector(selectors.BaseSelector):
    """Composes a stdlib selector + wasi-pollable tracking.

    The stdlib selector is constructed lazily on first real-fd
    register, so particles that never use real fds don't pay for it
    (and don't fail on broken wasi-CPython selector implementations).
    """

    # Slice size for mixed-wait branch. 5 ms is short enough to not be
    # noticeable interactively, long enough to keep CPU under 1% on a
    # purely-waiting loop.
    _MIXED_SLICE_MS = 5

    def __init__(self):
        self._wasi_keys: dict[int, SelectorKey] = {}
        self._wasi_announced: set[int] = set()
        self._stdlib: selectors.BaseSelector | None = None
        self._stdlib_failed: Exception | None = None
        self._map = _HybridMapping(self)

    # -- side-channel: callers tell us a handle is a wasi pollable --

    def announce_wasi(self, pollable_handle: int) -> None:
        """Mark this integer as a wasi pollable, so the next
        `register()` routes it to the wasi side. Must be called
        BEFORE `loop.add_reader(pollable_handle, ...)`. Idempotent."""
        self._wasi_announced.add(pollable_handle)

    def revoke_wasi(self, pollable_handle: int) -> None:
        """Forget the announcement. Callers should invoke this when
        a wasi pollable is dropped — a re-used integer (rare but
        possible after long-running particles) would otherwise be
        mis-routed back to the wasi branch."""
        self._wasi_announced.discard(pollable_handle)

    # -- lazy stdlib selector --

    def _ensure_stdlib(self) -> selectors.BaseSelector:
        if self._stdlib_failed is not None:
            raise RuntimeError(
                "particle: real-fd I/O not available on this wasi-CPython "
                f"build (selector init failed: {self._stdlib_failed!r})"
            )
        if self._stdlib is None:
            try:
                self._stdlib = selectors.DefaultSelector()
            except Exception as e:
                self._stdlib_failed = e
                raise RuntimeError(
                    "particle: real-fd I/O not available on this wasi-CPython "
                    f"build (selector init failed: {e!r})"
                )
        return self._stdlib

    # -- selectors.BaseSelector contract --

    def register(self, fileobj, events, data=None):
        fd = self._fileobj_lookup(fileobj)
        if fd in self._wasi_announced:
            if fd in self._wasi_keys:
                raise KeyError(f"wasi pollable {fd} already registered")
            key = SelectorKey(fileobj, fd, events, data)
            self._wasi_keys[fd] = key
            return key
        return self._ensure_stdlib().register(fileobj, events, data)

    def unregister(self, fileobj):
        fd = self._fileobj_lookup(fileobj)
        if fd in self._wasi_keys:
            return self._wasi_keys.pop(fd)
        if self._stdlib is None:
            raise KeyError(f"{fileobj!r} not registered")
        return self._stdlib.unregister(fileobj)

    def modify(self, fileobj, events, data=None):
        fd = self._fileobj_lookup(fileobj)
        if fd in self._wasi_keys:
            key = self._wasi_keys[fd]
            if events != key.events or data is not key.data:
                new = SelectorKey(fileobj, fd, events, data)
                self._wasi_keys[fd] = new
                return new
            return key
        if self._stdlib is None:
            raise KeyError(f"{fileobj!r} not registered")
        return self._stdlib.modify(fileobj, events, data)

    def get_map(self):
        return self._map

    def close(self):
        self._wasi_keys.clear()
        self._wasi_announced.clear()
        if self._stdlib is not None:
            self._stdlib.close()
            self._stdlib = None

    # -- select: the composition logic --

    def select(self, timeout=None):
        has_wasi = bool(self._wasi_keys)
        has_fds = self._stdlib is not None and bool(self._stdlib.get_map())

        if not has_wasi and not has_fds:
            # asyncio occasionally calls select() with nothing registered
            # (e.g. when the scheduled queue has run-soon entries and the
            # selector is consulted only for a non-blocking poll). Don't
            # call into _io_poll([], None) — it'd deadlock — just return.
            return []
        if has_wasi and not has_fds:
            return self._select_wasi_only(timeout)
        if has_fds and not has_wasi:
            return self._stdlib.select(timeout)
        return self._select_both(timeout)

    def _select_wasi_only(self, timeout):
        handles = list(self._wasi_keys.keys())
        ready = _runtime_host._io_poll(handles, self._timeout_ms(timeout))
        out = []
        for h in ready:
            key = self._wasi_keys.get(h)
            if key is not None:
                out.append((key, key.events))
        return out

    def _select_both(self, timeout):
        """Slice both kinds. Worst-case wakeup latency = _MIXED_SLICE_MS.

        Pattern: poll wasi pollables with a short timeout, then
        non-blocking sample the stdlib selector. If either reports
        readiness, return. Otherwise repeat until the overall
        `timeout` is exhausted.

        Why not the other way around (stdlib first, then wasi)? Either
        works, but blocking on wasi is what `_io_poll` is designed for;
        stdlib's PollSelector also blocks. Splitting either direction
        gives the same slice latency. Wasi-first keeps HTTP-heavy
        particles' wake-up tight; the stdlib side eats the slice.
        """
        out: list = []
        end = time.monotonic() + timeout if (timeout is not None and timeout > 0) else None
        non_blocking = (timeout == 0)

        while True:
            # Bound wasi wait to the smaller of the slice and the
            # caller's remaining timeout.
            slice_ms = self._MIXED_SLICE_MS
            if end is not None:
                remaining = end - time.monotonic()
                if remaining <= 0:
                    return out
                slice_ms = min(slice_ms, max(1, int(remaining * 1000)))
            if non_blocking:
                slice_ms = 0

            handles = list(self._wasi_keys.keys())
            ready = _runtime_host._io_poll(handles, slice_ms)
            for h in ready:
                key = self._wasi_keys.get(h)
                if key is not None:
                    out.append((key, key.events))

            ready_std = self._stdlib.select(0)  # always non-blocking sample
            out.extend(ready_std)

            if out:
                return out
            if non_blocking:
                return out
            if end is not None and time.monotonic() >= end:
                return out

    # -- helpers --

    @staticmethod
    def _timeout_ms(timeout):
        if timeout is None:
            return None
        if timeout <= 0:
            return 0
        # Round up so a 0.5 ms timeout doesn't truncate to 0.
        return max(1, int(timeout * 1000))

    @staticmethod
    def _fileobj_lookup(fileobj):
        try:
            return int(fileobj)
        except (TypeError, ValueError):
            return fileobj.fileno()


# -- event loop -------------------------------------------------------------

class WasiEventLoop(asyncio.SelectorEventLoop):
    """`SelectorEventLoop` with two surgical changes:

      - Constructed with `HybridSelector()` so wasi pollables and
        real fds both flow through one place.
      - `_make_self_pipe` / `_write_to_self` / `_close_self_pipe` are
        no-ops because `socket.socketpair()` doesn't exist on wasi
        (and we don't need cross-thread wakeup — wasi is single-
        threaded).

    Everything else inherits from upstream. `asyncio.sleep`, `gather`,
    `wait_for`, `Queue`, `Event`, etc. work exactly as documented.
    """

    def __init__(self):
        super().__init__(selector=HybridSelector())

    def _make_self_pipe(self):
        self._ssock = None
        self._csock = None

    def _close_self_pipe(self):
        pass

    def _write_to_self(self):
        # Called by call_soon_threadsafe / signal handlers to nudge the
        # selector. There are no other threads on wasi, and signals
        # don't reach guest code, so nothing has to nudge us — the
        # loop's normal call_soon queue is the only path.
        pass

    def _read_from_self(self):
        pass


class WasiEventLoopPolicy(asyncio.DefaultEventLoopPolicy):
    """Make `asyncio.run(...)` and `asyncio.new_event_loop()` produce
    a `WasiEventLoop`. Without this, the platform default factory
    (e.g. `_UnixSelectorEventLoop` on linux-CPython, even though
    we're on wasi-CPython) traps in __init__ at `socket.socketpair`."""

    _loop_factory = WasiEventLoop


# -- http future ------------------------------------------------------------

class WasiHttpFuture(asyncio.Future):
    """An asyncio.Future driving one wasi:http request.

    Lifecycle:
      1. `__init__` calls `_http_submit` → http handle, then
         `_http_pollable(handle)` → pollable handle. The pollable is
         announced to the selector (so `loop.add_reader` routes it to
         the wasi branch) and registered as a reader.
      2. When the pollable fires, `_on_ready` is invoked. It calls
         `_http_advance` to step the state machine. Still pending →
         re-arm a fresh pollable for the next state (e.g. the body
         stream). Done → call `_http_complete` and `set_result`.
      3. On cancel or GC, `_cleanup` releases the in-flight handle
         and revokes any outstanding pollable announcement.

    The result is the same `{"status", "headers", "body"}` dict the
    sync `_http_request` returns — `particle.http.async_fetch` wraps
    it into a `Response`.
    """

    def __init__(self, method, url, headers, body, *, loop=None):
        if loop is None:
            loop = asyncio.get_event_loop()
        super().__init__(loop=loop)
        self._handle: int | None = None
        self._pollable: int | None = None
        try:
            self._handle = _runtime_host._http_submit(
                method.upper() if isinstance(method, str) else method,
                url,
                headers,
                body,
            )
            self._arm_next_pollable()
        except BaseException as e:
            self._cleanup()
            self.set_exception(e)

    def _arm_next_pollable(self):
        self._pollable = _runtime_host._http_pollable(self._handle)
        # Announce BEFORE add_reader: add_reader internally calls
        # selector.register, which uses the announcement to route to
        # the wasi branch instead of treating it as a real fd.
        sel = self._loop._selector
        if hasattr(sel, "announce_wasi"):
            sel.announce_wasi(self._pollable)
        self._loop.add_reader(self._pollable, self._on_ready)

    def _on_ready(self):
        pollable = self._pollable
        if pollable is not None:
            self._loop.remove_reader(pollable)
            sel = self._loop._selector
            if hasattr(sel, "revoke_wasi"):
                sel.revoke_wasi(pollable)
            try:
                _runtime_host._pollable_drop(pollable)
            except Exception:
                pass
            self._pollable = None

        if self.cancelled():
            self._cleanup()
            return

        try:
            done = _runtime_host._http_advance(self._handle)
        except BaseException as e:
            self._cleanup()
            if not self.done():
                self.set_exception(e)
            return

        if done == 1:
            try:
                result = _runtime_host._http_complete(self._handle)
            except BaseException as e:
                self._cleanup()
                if not self.done():
                    self.set_exception(e)
                return
            self._handle = None  # _http_complete drops the entry host-side
            if not self.done():
                self.set_result(result)
        else:
            try:
                self._arm_next_pollable()
            except BaseException as e:
                self._cleanup()
                if not self.done():
                    self.set_exception(e)

    def _cleanup(self):
        if self._pollable is not None:
            try:
                self._loop.remove_reader(self._pollable)
            except Exception:
                pass
            sel = getattr(self._loop, "_selector", None)
            if sel is not None and hasattr(sel, "revoke_wasi"):
                try:
                    sel.revoke_wasi(self._pollable)
                except Exception:
                    pass
            try:
                _runtime_host._pollable_drop(self._pollable)
            except Exception:
                pass
            self._pollable = None
        if self._handle is not None:
            try:
                _runtime_host._http_drop(self._handle)
            except Exception:
                pass
            self._handle = None

    def cancel(self, msg=None):
        self._cleanup()
        return super().cancel(msg=msg)

    def __del__(self):
        try:
            self._cleanup()
        except Exception:
            pass


# -- policy install ---------------------------------------------------------

_installed = False


def install_policy() -> None:
    """Install `WasiEventLoopPolicy` as the default. Called once from
    `particle.__init__` so any user `asyncio.run(...)` picks it up
    without per-particle ceremony. Idempotent."""
    global _installed
    if _installed:
        return
    asyncio.set_event_loop_policy(WasiEventLoopPolicy())
    _installed = True
