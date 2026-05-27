//! Particle Python runtime — Rust shim that owns the
//! particle:runtime/{tools, health, manifest} WIT exports and
//! dispatches into a Python `bootstrap.py` through the CPython C API.
//!
//! Architecture:
//!
//!   - This crate compiles to a staticlib that the Makefile hand-
//!     links into a PIC side module (`main.wasm`). main.wasm imports
//!     libpython3.14.so / libc.so / libdl.so / wasi-emulated-* / the
//!     init_dyld.so sibling, all composed at component-link time via
//!     `wasm-tools component link --dl-openable`.
//!   - libdl.so is supplied by components/dyld-libdl/ — its
//!     dlopen/dlsym/dlclose/dlerror route through
//!     `particle:host/dyld@0.1.0`.
//!   - inject-capture post-processes the composed component so the
//!     host's `dyld.initialize` handler can capture the union of
//!     every embedded module's exports.
//!
//! This file's scope is intentionally tight: bring Python up, load
//! bootstrap.py, dispatch list-tools / call-tool / ping / get-manifest
//! to it, and marshal results back. The hard variant marshalling
//! (manifest credentials/methods) is stubbed with an error return for
//! the initial skeleton; once the pipeline is green end-to-end we'll
//! iterate it in.

mod async_http;
mod host_module;

// `append_to_inittab!` is an ident-only macro — it can't take a `path::name`
// — so re-export the pymodule into this scope so the macro sees a bare ident.
use host_module::_runtime_host;

use core::ffi::{c_char, c_int, c_void, CStr};
use std::ffi::CString;
use std::sync::Once;

/// Local convenience: pyo3-ffi exposes `Py_None()` as an `unsafe fn`
/// but no shorthand for the identity check. One-liner so the
/// dispatcher code stays readable.
#[inline]
unsafe fn is_none(obj: *mut pyo3_ffi::PyObject) -> bool {
    obj == pyo3_ffi::Py_None()
}

wit_bindgen::generate!({
    world: "python-runtime",
    path: "wit",
    generate_all,
});

use exports::particle::runtime::health::{
    ErrorDetail as HealthErrorDetail, Guest as HealthGuest, HealthError, PingResult, Status,
};
use exports::particle::runtime::manifest::{
    ApikeyLocation, ApikeyLocationKind, ApikeyMethod, CapabilitySet, CredentialEntry,
    CredentialMethod, CredentialMethodEntry, ErrorDetail as ManifestErrorDetail, Guest as
    ManifestGuest, HttpCapability, ManifestError, Oauth2Flow, Oauth2Method, ParticleManifest,
    SigningAlgorithm, SigningKeyMethod, ToolEntry,
};
use exports::particle::runtime::tools::{
    ErrorDetail as ToolErrorDetail, Guest as ToolsGuest, ToolDef, ToolError,
};

use particle::host::dyld::{self as host_dyld, PreloadedLibrary, PreloadedSymbol};

// -- libdl interface (resolved at composition time via libdl.so) -----------

// libdl.so (components/dyld-libdl built with `canonical-dyld`) exports
// these. wasm-ld emits them as env imports; `wasm-tools component
// link` wires them to libdl.so's exports during composition.
//
// `dlerror` is the only one this skeleton invokes (via last_dlerror).
// The other three are kept for iteration N+1 — once we wire native
// extension loading via dlopen+dlsym(PyInit_*) we'll surface them.
#[allow(dead_code)]
extern "C" {
    fn dlopen(name: *const c_char, flags: c_int) -> *mut c_void;
    fn dlsym(handle: *mut c_void, sym: *const c_char) -> *mut c_void;
    fn dlclose(handle: *mut c_void) -> c_int;
    fn dlerror() -> *const c_char;
}

#[allow(dead_code)]
fn last_dlerror() -> String {
    let p = unsafe { dlerror() };
    if p.is_null() {
        "(no dlerror message)".into()
    } else {
        unsafe { CStr::from_ptr(p) }.to_string_lossy().into_owned()
    }
}

// -- __wasm_set_libraries shim --------------------------------------------
//
// The wasm-tools-generated init module's start fn calls this with a
// pointer into our linear memory; we deserialize the LIBRARIES table
// (format documented in wit-component's linking.rs:155-163) and hand
// it to the host's dyld.set-libraries.

#[no_mangle]
pub unsafe extern "C" fn __wasm_set_libraries(ptr: *const u8) {
    if ptr.is_null() {
        return;
    }
    let libraries = read_libraries(ptr);
    let _ = host_dyld::set_libraries(&libraries);
}

unsafe fn read_libraries(ptr: *const u8) -> Vec<PreloadedLibrary> {
    let count = read_u32(ptr);
    let lib_array_ptr = read_u32(ptr.add(4)) as *const u8;
    let mut out = Vec::with_capacity(count as usize);
    for i in 0..count {
        let lib_ptr = lib_array_ptr.add((i as usize) * 16);
        let name = read_name(lib_ptr);
        let sym_count = read_u32(lib_ptr.add(8));
        let sym_array_ptr = read_u32(lib_ptr.add(12)) as *const u8;
        let mut symbols = Vec::with_capacity(sym_count as usize);
        for j in 0..sym_count {
            let sym_ptr = sym_array_ptr.add((j as usize) * 12);
            symbols.push(PreloadedSymbol {
                name: read_name(sym_ptr),
                address: read_u32(sym_ptr.add(8)),
            });
        }
        out.push(PreloadedLibrary { name, symbols });
    }
    out
}

#[inline]
unsafe fn read_u32(p: *const u8) -> u32 {
    core::ptr::read_unaligned(p as *const u32)
}

unsafe fn read_name(p: *const u8) -> String {
    let length = read_u32(p) as usize;
    let data = read_u32(p.add(4)) as *const u8;
    let slice = core::slice::from_raw_parts(data, length);
    // libdl LIBRARIES names are valid UTF-8 identifiers; lossy is
    // defense in depth, not a real concern.
    String::from_utf8_lossy(slice).into_owned()
}

// -- Python startup --------------------------------------------------------
//
// Lazy first-call init: Py_Initialize, mutate sys.path to find
// /particle/bootstrap.py (host-mounted via wasi preopen), import
// bootstrap. The `Once` guard keeps reentrant calls into the WIT
// exports from re-running initialization.

static INIT: Once = Once::new();
static mut BOOTSTRAP: *mut pyo3_ffi::PyObject = std::ptr::null_mut();
static mut INIT_ERROR: Option<String> = None;

/// Where the host stages the runtime files (bootstrap.py + particle/
/// package). The host mounts this directory as a wasi:filesystem
/// preopen at component-instantiation time. Iteration N+1 will switch
/// to a frozen-stdlib zip embedded via go:embed; iteration N (this
/// skeleton) reads the files directly from the preopen.
const RUNTIME_DIR: &str = "/runtime";

/// Where the host mounts the user's particle bundle. The bootstrap
/// loads `/particle/bundle.py` through importlib.SourceFileLoader on
/// first tool call. Defined here for documentation; the actual path
/// reference lives Python-side in bootstrap.py for the skeleton.
#[allow(dead_code)]
const PARTICLE_DIR: &str = "/particle";

fn ensure_python_initialized() -> Result<*mut pyo3_ffi::PyObject, String> {
    INIT.call_once(|| unsafe {
        // Register the PyO3-built `_runtime_host` extension module with
        // CPython's import machinery BEFORE Py_Initialize. After Initialize
        // runs, PyImport_AppendInittab is a no-op (the inittab freeze has
        // happened) so we'd silently lose the registration if we deferred.
        // `append_to_inittab!` is PyO3's wrapper that names the C-side
        // `PyInit__runtime_host` symbol for us.
        pyo3::append_to_inittab!(_runtime_host);

        // Py_Initialize sets up the interpreter — sys.path, sys.modules,
        // builtins. We deliberately avoid Py_InitializeFromConfig for
        // the skeleton: the default config is enough to import a
        // SourceFileLoader-loaded module, and PyConfig's struct layout
        // is fragile across CPython minor versions. Once everything
        // works end-to-end we can revisit to disable site, set the
        // home dir explicitly, etc.
        pyo3_ffi::Py_Initialize();

        // Patch sys.path so `import bootstrap` finds /runtime/bootstrap.py.
        // PyRun_SimpleStringFlags is the path of least resistance —
        // PyList_Insert plus a sys.path lookup would be marginally
        // tidier but more FFI calls. We escape the path as a JSON
        // string for safety (Python and JSON agree on string escapes
        // for ASCII paths, which RUNTIME_DIR always is).
        let setup_src = format!(
            "import sys\n\
             _p = \"{path}\"\n\
             if _p not in sys.path:\n\
             \x20\x20\x20\x20sys.path.insert(0, _p)\n\
             del _p\n",
            path = RUNTIME_DIR,
        );
        let setup = CString::new(setup_src).unwrap();
        if pyo3_ffi::PyRun_SimpleStringFlags(setup.as_ptr(), std::ptr::null_mut()) != 0 {
            pyo3_ffi::PyErr_Print();
            INIT_ERROR = Some("sys.path setup failed".into());
            return;
        }

        // import bootstrap
        let mod_name = CString::new("bootstrap").unwrap();
        let bootstrap = pyo3_ffi::PyImport_ImportModule(mod_name.as_ptr());
        if bootstrap.is_null() {
            INIT_ERROR = Some(fetch_pyerr("import bootstrap"));
            return;
        }
        BOOTSTRAP = bootstrap;
    });

    unsafe {
        if let Some(ref e) = INIT_ERROR {
            return Err(e.clone());
        }
        if BOOTSTRAP.is_null() {
            return Err("bootstrap module not initialized".into());
        }
        Ok(BOOTSTRAP)
    }
}

/// Fetch the active Python exception (if any) as a single-line summary.
/// Clears the error indicator so subsequent C-API calls don't see a
/// stale pending exception.
unsafe fn fetch_pyerr(context: &str) -> String {
    // PyErr_GetRaisedException both takes the indicator (clearing it)
    // and returns the already-normalized exception instance. Replaces
    // the deprecated PyErr_Fetch + PyErr_NormalizeException pair.
    let exc = pyo3_ffi::PyErr_GetRaisedException();
    if exc.is_null() {
        return format!("{context}: no Python exception set");
    }

    // Best-effort message — str(exc) gives us the exception's __str__.
    let mut summary = format!("{context}: ");
    let s = pyo3_ffi::PyObject_Str(exc);
    if !s.is_null() {
        let c = pyo3_ffi::PyUnicode_AsUTF8(s);
        if !c.is_null() {
            summary.push_str(&CStr::from_ptr(c).to_string_lossy());
        }
        pyo3_ffi::Py_DecRef(s);
    }

    // Side-channel: also display the full traceback to stderr so the
    // host sees the operator-visible stack alongside the WIT-level
    // summary. PyErr_DisplayException doesn't touch the indicator
    // (which we've already cleared), making it the right primitive
    // when you already own the exc reference.
    pyo3_ffi::PyErr_DisplayException(exc);
    pyo3_ffi::Py_DecRef(exc);
    summary
}

// -- helpers for the export impls ------------------------------------------

/// Read a UTF-8 string attribute off `obj` and return it as a Rust
/// `String`. Returns None if the attribute is missing or not a string.
unsafe fn get_string_attr(obj: *mut pyo3_ffi::PyObject, name: &str) -> Option<String> {
    let cname = CString::new(name).ok()?;
    let attr = pyo3_ffi::PyObject_GetAttrString(obj, cname.as_ptr());
    if attr.is_null() {
        pyo3_ffi::PyErr_Clear();
        return None;
    }
    let cstr = pyo3_ffi::PyUnicode_AsUTF8(attr);
    let result = if cstr.is_null() {
        None
    } else {
        Some(CStr::from_ptr(cstr).to_string_lossy().into_owned())
    };
    pyo3_ffi::Py_DecRef(attr);
    result
}

/// Call `bootstrap.<class_name>()` and return the resulting instance.
/// Caller owns the reference and must DecRef when done.
unsafe fn instantiate(bootstrap: *mut pyo3_ffi::PyObject, class_name: &str) -> Result<*mut pyo3_ffi::PyObject, String> {
    let cname = CString::new(class_name).map_err(|e| e.to_string())?;
    let class = pyo3_ffi::PyObject_GetAttrString(bootstrap, cname.as_ptr());
    if class.is_null() {
        return Err(fetch_pyerr(&format!("getattr(bootstrap, {class_name:?})")));
    }
    let instance = pyo3_ffi::PyObject_CallNoArgs(class);
    pyo3_ffi::Py_DecRef(class);
    if instance.is_null() {
        return Err(fetch_pyerr(&format!("{class_name}()")));
    }
    Ok(instance)
}

/// Call `obj.<method_name>()` with no arguments.
unsafe fn call_method_noargs(
    obj: *mut pyo3_ffi::PyObject,
    method_name: &str,
) -> Result<*mut pyo3_ffi::PyObject, String> {
    let cname = CString::new(method_name).map_err(|e| e.to_string())?;
    let method = pyo3_ffi::PyObject_GetAttrString(obj, cname.as_ptr());
    if method.is_null() {
        return Err(fetch_pyerr(&format!("getattr({method_name:?})")));
    }
    let result = pyo3_ffi::PyObject_CallNoArgs(method);
    pyo3_ffi::Py_DecRef(method);
    if result.is_null() {
        return Err(fetch_pyerr(method_name));
    }
    Ok(result)
}

/// Build a PyTuple of two PyUnicode strings. Used by the two-arg
/// `Tools.call_tool(name, args_json)` dispatch.
unsafe fn build_two_string_tuple(a: &str, b: &str) -> Result<*mut pyo3_ffi::PyObject, String> {
    let pa = pyunicode_from_str(a)?;
    let pb = pyunicode_from_str(b).map_err(|e| {
        pyo3_ffi::Py_DecRef(pa);
        e
    })?;
    let tup = pyo3_ffi::PyTuple_New(2);
    if tup.is_null() {
        pyo3_ffi::Py_DecRef(pa);
        pyo3_ffi::Py_DecRef(pb);
        return Err(fetch_pyerr("PyTuple_New(2)"));
    }
    // PyTuple_SetItem STEALS both references on success.
    if pyo3_ffi::PyTuple_SetItem(tup, 0, pa) != 0 {
        pyo3_ffi::Py_DecRef(tup);
        pyo3_ffi::Py_DecRef(pb);
        return Err(fetch_pyerr("PyTuple_SetItem(0)"));
    }
    if pyo3_ffi::PyTuple_SetItem(tup, 1, pb) != 0 {
        pyo3_ffi::Py_DecRef(tup);
        return Err(fetch_pyerr("PyTuple_SetItem(1)"));
    }
    Ok(tup)
}

/// Construct a new PyUnicode from a Rust string slice. Handles NULs
/// embedded in the slice by truncating to the leading NUL-free run is
/// undesirable; we use PyUnicode_FromStringAndSize so embedded NULs go
/// through (Python's str supports them, even if JSON typically won't).
unsafe fn pyunicode_from_str(s: &str) -> Result<*mut pyo3_ffi::PyObject, String> {
    let p = pyo3_ffi::PyUnicode_FromStringAndSize(s.as_ptr() as *const c_char, s.len() as isize);
    if p.is_null() {
        return Err(fetch_pyerr("PyUnicode_FromStringAndSize"));
    }
    Ok(p)
}

/// Extract a UTF-8 string from a PyUnicode. Returns None if the
/// object isn't a str or the extraction fails. The returned `String`
/// is owned; the PyObject's lifetime can end after this call.
unsafe fn pyunicode_to_string(obj: *mut pyo3_ffi::PyObject) -> Option<String> {
    if obj.is_null() {
        return None;
    }
    let mut size: isize = 0;
    let c = pyo3_ffi::PyUnicode_AsUTF8AndSize(obj, &mut size);
    if c.is_null() {
        // PyUnicode_AsUTF8AndSize sets a TypeError when obj isn't str.
        // Clear it — callers may try alternate paths.
        pyo3_ffi::PyErr_Clear();
        return None;
    }
    let bytes = core::slice::from_raw_parts(c as *const u8, size as usize);
    Some(String::from_utf8_lossy(bytes).into_owned())
}

/// Optional string attribute — returns None when the attribute exists
/// but is Python's `None`, when it's missing entirely, or when it's
/// not convertible to UTF-8.
unsafe fn get_optional_string_attr(obj: *mut pyo3_ffi::PyObject, name: &str) -> Option<String> {
    let cname = CString::new(name).ok()?;
    let attr = pyo3_ffi::PyObject_GetAttrString(obj, cname.as_ptr());
    if attr.is_null() {
        pyo3_ffi::PyErr_Clear();
        return None;
    }
    let result = if is_none(attr) {
        None
    } else {
        pyunicode_to_string(attr)
    };
    pyo3_ffi::Py_DecRef(attr);
    result
}

/// Get a bool-coerced attribute. Missing attributes return `default`.
unsafe fn get_bool_attr(obj: *mut pyo3_ffi::PyObject, name: &str, default: bool) -> bool {
    let cname = match CString::new(name) {
        Ok(c) => c,
        Err(_) => return default,
    };
    let attr = pyo3_ffi::PyObject_GetAttrString(obj, cname.as_ptr());
    if attr.is_null() {
        pyo3_ffi::PyErr_Clear();
        return default;
    }
    let r = pyo3_ffi::PyObject_IsTrue(attr);
    pyo3_ffi::Py_DecRef(attr);
    match r {
        1 => true,
        0 => false,
        _ => {
            pyo3_ffi::PyErr_Clear();
            default
        }
    }
}

/// Iterate a Python list/iterable attribute, applying `f` to each
/// (borrowed) item. Returns the collected Vec, or an error if iteration
/// itself fails. Missing attribute => empty Vec.
unsafe fn get_list_attr<T>(
    obj: *mut pyo3_ffi::PyObject,
    name: &str,
    mut f: impl FnMut(*mut pyo3_ffi::PyObject) -> Result<T, String>,
) -> Result<Vec<T>, String> {
    let cname = CString::new(name).map_err(|e| e.to_string())?;
    let attr = pyo3_ffi::PyObject_GetAttrString(obj, cname.as_ptr());
    if attr.is_null() {
        pyo3_ffi::PyErr_Clear();
        return Ok(Vec::new());
    }
    if is_none(attr) {
        pyo3_ffi::Py_DecRef(attr);
        return Ok(Vec::new());
    }
    let iter = pyo3_ffi::PyObject_GetIter(attr);
    pyo3_ffi::Py_DecRef(attr);
    if iter.is_null() {
        return Err(fetch_pyerr(&format!("PyObject_GetIter({name})")));
    }
    let mut out = Vec::new();
    loop {
        let item = pyo3_ffi::PyIter_Next(iter);
        if item.is_null() {
            // Either end of iteration or an error.
            if !pyo3_ffi::PyErr_Occurred().is_null() {
                let msg = fetch_pyerr(&format!("PyIter_Next({name})"));
                pyo3_ffi::Py_DecRef(iter);
                return Err(msg);
            }
            break;
        }
        let r = f(item);
        pyo3_ffi::Py_DecRef(item);
        out.push(r?);
    }
    pyo3_ffi::Py_DecRef(iter);
    Ok(out)
}

/// Structured form of a fetched Python exception. The `kind` field is
/// the bootstrap-provided variant discriminator (e.g. "not-found",
/// "invalid-arguments") when the raised class set one; otherwise None.
struct PyExcInfo {
    class_name: String,
    kind: Option<String>,
    message: String,
    stack: Option<String>,
}

/// Fetch the current Python exception (if any) and split it into a
/// PyExcInfo. Clears the error indicator. The dispatchers below match
/// on `kind` first (set by bootstrap classes) and on `class_name`
/// second.
unsafe fn fetch_pyerr_structured() -> Option<PyExcInfo> {
    // PyErr_GetRaisedException clears the indicator and returns the
    // already-normalized exception instance — the traceback is
    // attached on it (`exc.__traceback__`), so we don't have to carry
    // it separately the way the old PyErr_Fetch triple required.
    let exc = pyo3_ffi::PyErr_GetRaisedException();
    if exc.is_null() {
        return None;
    }

    // Class name from type(exc).__name__. PyObject_Type returns a
    // strong ref to the type object.
    let etype = pyo3_ffi::PyObject_Type(exc);
    let class_name = if !etype.is_null() {
        let nm = CString::new("__name__").unwrap();
        let n = pyo3_ffi::PyObject_GetAttrString(etype, nm.as_ptr());
        if !n.is_null() {
            let s = pyunicode_to_string(n).unwrap_or_else(|| "<exception>".into());
            pyo3_ffi::Py_DecRef(n);
            s
        } else {
            pyo3_ffi::PyErr_Clear();
            "<exception>".into()
        }
    } else {
        "<exception>".into()
    };

    let kind = get_optional_string_attr(exc, "kind");
    let mut message = if let Some(m) = get_optional_string_attr(exc, "message") {
        m
    } else {
        let s = pyo3_ffi::PyObject_Str(exc);
        if !s.is_null() {
            let r = pyunicode_to_string(s).unwrap_or_default();
            pyo3_ffi::Py_DecRef(s);
            r
        } else {
            pyo3_ffi::PyErr_Clear();
            String::new()
        }
    };
    if message.is_empty() {
        message = class_name.clone();
    }

    let stack = if let Some(s) = get_optional_string_attr(exc, "stack") {
        Some(s)
    } else {
        format_traceback(exc)
    };

    if !etype.is_null() {
        pyo3_ffi::Py_DecRef(etype);
    }
    pyo3_ffi::Py_DecRef(exc);

    Some(PyExcInfo {
        class_name,
        kind,
        message,
        stack,
    })
}

/// Format a Python traceback via the `traceback` module. Returns None
/// on any failure (best-effort diagnostic). Reads `exc.__traceback__`
/// implicitly — PyErr_GetRaisedException leaves the traceback attached
/// to the instance, so the single-argument form of
/// `traceback.format_exception` is all we need.
unsafe fn format_traceback(exc: *mut pyo3_ffi::PyObject) -> Option<String> {
    if exc.is_null() {
        return None;
    }
    let modname = CString::new("traceback").ok()?;
    let tb_mod = pyo3_ffi::PyImport_ImportModule(modname.as_ptr());
    if tb_mod.is_null() {
        pyo3_ffi::PyErr_Clear();
        return None;
    }
    let fname = CString::new("format_exception").ok()?;
    let func = pyo3_ffi::PyObject_GetAttrString(tb_mod, fname.as_ptr());
    pyo3_ffi::Py_DecRef(tb_mod);
    if func.is_null() {
        pyo3_ffi::PyErr_Clear();
        return None;
    }

    // Python 3.10+: `traceback.format_exception(exc, /)` accepts the
    // exception alone and pulls type + traceback off the instance.
    // We're on 3.14 so the single-arg form is guaranteed.
    let args = pyo3_ffi::PyTuple_New(1);
    if args.is_null() {
        pyo3_ffi::Py_DecRef(func);
        pyo3_ffi::PyErr_Clear();
        return None;
    }
    pyo3_ffi::Py_IncRef(exc);
    pyo3_ffi::PyTuple_SetItem(args, 0, exc);
    let lines = pyo3_ffi::PyObject_CallObject(func, args);
    pyo3_ffi::Py_DecRef(args);
    pyo3_ffi::Py_DecRef(func);
    if lines.is_null() {
        pyo3_ffi::PyErr_Clear();
        return None;
    }
    // lines is a list[str]; concatenate.
    let n = pyo3_ffi::PyList_Size(lines);
    let mut out = String::new();
    for i in 0..n {
        let item = pyo3_ffi::PyList_GetItem(lines, i); // borrowed
        if let Some(s) = pyunicode_to_string(item) {
            out.push_str(&s);
        }
    }
    pyo3_ffi::Py_DecRef(lines);
    if out.is_empty() {
        None
    } else {
        Some(out)
    }
}

// -- export impls ----------------------------------------------------------

struct Component;

impl ToolsGuest for Component {
    fn list_tools() -> Vec<ToolDef> {
        let result = unsafe { do_list_tools() };
        match result {
            Ok(v) => v,
            Err(e) => {
                // list-tools has no error variant — surface load
                // failures as a synthetic tool row, same shape the
                // componentize-py bootstrap used.
                vec![ToolDef {
                    name: "__particle_load_error__".into(),
                    description: e,
                    input_schema_json: "{}".into(),
                }]
            }
        }
    }

    fn call_tool(name: String, arguments_json: String) -> Result<String, ToolError> {
        unsafe { do_call_tool(&name, &arguments_json) }
    }
}

unsafe fn do_list_tools() -> Result<Vec<ToolDef>, String> {
    let bootstrap = ensure_python_initialized()?;
    let tools_inst = instantiate(bootstrap, "Tools")?;
    let result = call_method_noargs(tools_inst, "list_tools");
    pyo3_ffi::Py_DecRef(tools_inst);
    let py_list = result?;

    let mut out = Vec::new();
    let len = pyo3_ffi::PyList_Size(py_list);
    if len < 0 {
        pyo3_ffi::Py_DecRef(py_list);
        return Err(fetch_pyerr("list_tools: PyList_Size"));
    }
    for i in 0..len {
        // PyList_GetItem returns a BORROWED reference — no DecRef.
        let item = pyo3_ffi::PyList_GetItem(py_list, i);
        if item.is_null() {
            pyo3_ffi::Py_DecRef(py_list);
            return Err(fetch_pyerr(&format!("list_tools[{i}] is NULL")));
        }
        let name = get_string_attr(item, "name").unwrap_or_default();
        let description = get_string_attr(item, "description").unwrap_or_default();
        let input_schema_json =
            get_string_attr(item, "input_schema_json").unwrap_or_else(|| "{}".into());
        out.push(ToolDef {
            name,
            description,
            input_schema_json,
        });
    }
    pyo3_ffi::Py_DecRef(py_list);
    Ok(out)
}

unsafe fn do_call_tool(name: &str, arguments_json: &str) -> Result<String, ToolError> {
    let bootstrap = ensure_python_initialized().map_err(|e| {
        ToolError::HandlerError(ToolErrorDetail {
            message: e,
            stack: None,
        })
    })?;
    let tools_inst = instantiate(bootstrap, "Tools").map_err(|e| {
        ToolError::HandlerError(ToolErrorDetail {
            message: e,
            stack: None,
        })
    })?;

    // Build the (name, arguments_json) tuple and dispatch through
    // PyObject_CallObject on the bound method. Equivalent to
    // `tools_inst.call_tool(name, arguments_json)` from Python.
    let method = {
        let cn = CString::new("call_tool").unwrap();
        pyo3_ffi::PyObject_GetAttrString(tools_inst, cn.as_ptr())
    };
    pyo3_ffi::Py_DecRef(tools_inst);
    if method.is_null() {
        return Err(ToolError::HandlerError(ToolErrorDetail {
            message: fetch_pyerr("getattr(call_tool)"),
            stack: None,
        }));
    }

    let args = match build_two_string_tuple(name, arguments_json) {
        Ok(t) => t,
        Err(e) => {
            pyo3_ffi::Py_DecRef(method);
            return Err(ToolError::HandlerError(ToolErrorDetail {
                message: e,
                stack: None,
            }));
        }
    };
    let py_result = pyo3_ffi::PyObject_CallObject(method, args);
    pyo3_ffi::Py_DecRef(args);
    pyo3_ffi::Py_DecRef(method);

    if py_result.is_null() {
        // Exception path. Inspect the raised type and route to the
        // appropriate ToolError variant.
        let info = fetch_pyerr_structured().unwrap_or_else(|| PyExcInfo {
            class_name: "<unknown>".into(),
            kind: None,
            message: "call_tool: no Python exception set".into(),
            stack: None,
        });
        return Err(tool_error_from(info));
    }

    // Success: the Python bootstrap returns a str. Extract UTF-8.
    let out = pyunicode_to_string(py_result);
    pyo3_ffi::Py_DecRef(py_result);
    match out {
        Some(s) => Ok(s),
        None => Err(ToolError::HandlerError(ToolErrorDetail {
            message: "call_tool: handler return was not a str".into(),
            stack: None,
        })),
    }
}

/// Map a PyExcInfo to a ToolError variant. Dispatch order: explicit
/// `kind` attribute, then class name, then fall back to handler-error.
fn tool_error_from(info: PyExcInfo) -> ToolError {
    let detail = ToolErrorDetail {
        message: info.message,
        stack: info.stack,
    };
    let tag = info.kind.as_deref().unwrap_or(info.class_name.as_str());
    match tag {
        "not-found" | "NotFound" => ToolError::NotFound,
        "invalid-arguments" | "InvalidArguments" => ToolError::InvalidArguments(detail),
        "capability-denied" | "CapabilityDenied" => ToolError::CapabilityDenied(detail),
        "handler-error" | "HandlerError" => ToolError::HandlerError(detail),
        _ => ToolError::HandlerError(detail),
    }
}

impl HealthGuest for Component {
    fn ping() -> Result<PingResult, HealthError> {
        unsafe { do_ping() }
    }
}

unsafe fn do_ping() -> Result<PingResult, HealthError> {
    let bootstrap = ensure_python_initialized().map_err(|e| {
        HealthError::HandlerError(HealthErrorDetail {
            message: e,
            stack: None,
        })
    })?;
    let inst = instantiate(bootstrap, "Health").map_err(|e| {
        HealthError::HandlerError(HealthErrorDetail {
            message: e,
            stack: None,
        })
    })?;

    // Inline the call (rather than call_method_noargs) so we get the
    // structured exception info on the error path — the helper clears
    // the indicator with PyErr_Print, which we don't want here.
    let method = {
        let cn = CString::new("ping").unwrap();
        pyo3_ffi::PyObject_GetAttrString(inst, cn.as_ptr())
    };
    pyo3_ffi::Py_DecRef(inst);
    if method.is_null() {
        return Err(HealthError::HandlerError(HealthErrorDetail {
            message: fetch_pyerr("getattr(ping)"),
            stack: None,
        }));
    }
    let py_result = pyo3_ffi::PyObject_CallNoArgs(method);
    pyo3_ffi::Py_DecRef(method);
    if py_result.is_null() {
        let info = fetch_pyerr_structured().unwrap_or_else(|| PyExcInfo {
            class_name: "<unknown>".into(),
            kind: None,
            message: "ping: no Python exception set".into(),
            stack: None,
        });
        return Err(health_error_from(info));
    }

    // Success path: read .status (str), .message (option<str>),
    // .details (option<str>) off the returned object.
    let status_str = get_optional_string_attr(py_result, "status").unwrap_or_default();
    let message = get_optional_string_attr(py_result, "message");
    let details = get_optional_string_attr(py_result, "details");
    pyo3_ffi::Py_DecRef(py_result);

    let status = match status_str.as_str() {
        "ok" => Status::Ok,
        "degraded" => Status::Degraded,
        "unhealthy" => Status::Unhealthy,
        other => {
            return Err(HealthError::HandlerError(HealthErrorDetail {
                message: format!("ping returned unknown status {other:?}"),
                stack: None,
            }));
        }
    };
    Ok(PingResult {
        status,
        message,
        details,
    })
}

fn health_error_from(info: PyExcInfo) -> HealthError {
    let detail = HealthErrorDetail {
        message: info.message,
        stack: info.stack,
    };
    let tag = info.kind.as_deref().unwrap_or(info.class_name.as_str());
    match tag {
        "not-implemented" | "NotImplementedHealth" => HealthError::NotImplemented,
        _ => HealthError::HandlerError(detail),
    }
}

impl ManifestGuest for Component {
    fn get_manifest() -> Result<ParticleManifest, ManifestError> {
        unsafe { do_get_manifest() }
    }
}

unsafe fn do_get_manifest() -> Result<ParticleManifest, ManifestError> {
    let bootstrap = ensure_python_initialized().map_err(|e| {
        ManifestError::BundleLoadError(ManifestErrorDetail {
            message: e,
            stack: None,
        })
    })?;
    let inst = instantiate(bootstrap, "Manifest").map_err(|e| {
        ManifestError::BundleLoadError(ManifestErrorDetail {
            message: e,
            stack: None,
        })
    })?;

    let method = {
        let cn = CString::new("get_manifest").unwrap();
        pyo3_ffi::PyObject_GetAttrString(inst, cn.as_ptr())
    };
    pyo3_ffi::Py_DecRef(inst);
    if method.is_null() {
        return Err(ManifestError::BundleLoadError(ManifestErrorDetail {
            message: fetch_pyerr("getattr(get_manifest)"),
            stack: None,
        }));
    }
    let py_result = pyo3_ffi::PyObject_CallNoArgs(method);
    pyo3_ffi::Py_DecRef(method);
    if py_result.is_null() {
        let info = fetch_pyerr_structured().unwrap_or_else(|| PyExcInfo {
            class_name: "<unknown>".into(),
            kind: None,
            message: "get_manifest: no Python exception set".into(),
            stack: None,
        });
        let detail = ManifestErrorDetail {
            message: info.message,
            stack: info.stack,
        };
        let tag = info.kind.as_deref().unwrap_or(info.class_name.as_str());
        return Err(match tag {
            "invalid-manifest" => ManifestError::InvalidManifest(detail),
            _ => ManifestError::BundleLoadError(detail),
        });
    }

    // Marshal the ParticleManifest record.
    let r = marshal_particle_manifest(py_result);
    pyo3_ffi::Py_DecRef(py_result);
    r.map_err(|e| {
        ManifestError::InvalidManifest(ManifestErrorDetail {
            message: e,
            stack: None,
        })
    })
}

unsafe fn marshal_particle_manifest(obj: *mut pyo3_ffi::PyObject) -> Result<ParticleManifest, String> {
    let name = get_string_attr(obj, "name").unwrap_or_default();
    let description = get_string_attr(obj, "description").unwrap_or_default();
    let version = get_string_attr(obj, "version").unwrap_or_default();
    let capabilities = marshal_capability_set(obj)?;
    let credentials = get_list_attr(obj, "credentials", |item| {
        marshal_credential_entry(item)
    })?;
    let tools = get_list_attr(obj, "tools", |item| marshal_tool_entry(item))?;
    Ok(ParticleManifest {
        name,
        description,
        version,
        capabilities,
        credentials,
        tools,
    })
}

unsafe fn marshal_capability_set(parent: *mut pyo3_ffi::PyObject) -> Result<CapabilitySet, String> {
    // parent.capabilities — a CapabilitySet-like object with .http.
    let cn = CString::new("capabilities").unwrap();
    let caps = pyo3_ffi::PyObject_GetAttrString(parent, cn.as_ptr());
    if caps.is_null() {
        pyo3_ffi::PyErr_Clear();
        return Ok(CapabilitySet { http: None });
    }
    if is_none(caps) {
        pyo3_ffi::Py_DecRef(caps);
        return Ok(CapabilitySet { http: None });
    }
    let http_name = CString::new("http").unwrap();
    let http_obj = pyo3_ffi::PyObject_GetAttrString(caps, http_name.as_ptr());
    pyo3_ffi::Py_DecRef(caps);
    let http = if http_obj.is_null() {
        pyo3_ffi::PyErr_Clear();
        None
    } else if is_none(http_obj) {
        pyo3_ffi::Py_DecRef(http_obj);
        None
    } else {
        let allowed_hosts = get_list_attr(http_obj, "allowed_hosts", |item| {
            Ok(pyunicode_to_string(item).unwrap_or_default())
        })?;
        pyo3_ffi::Py_DecRef(http_obj);
        Some(HttpCapability { allowed_hosts })
    };
    Ok(CapabilitySet { http })
}

unsafe fn marshal_tool_entry(obj: *mut pyo3_ffi::PyObject) -> Result<ToolEntry, String> {
    Ok(ToolEntry {
        name: get_string_attr(obj, "name").unwrap_or_default(),
        description: get_string_attr(obj, "description").unwrap_or_default(),
        input_schema_json: get_string_attr(obj, "input_schema_json")
            .unwrap_or_else(|| "{}".into()),
    })
}

unsafe fn marshal_credential_entry(obj: *mut pyo3_ffi::PyObject) -> Result<CredentialEntry, String> {
    let name = get_string_attr(obj, "name").unwrap_or_default();
    let hosts = get_list_attr(obj, "hosts", |item| {
        Ok(pyunicode_to_string(item).unwrap_or_default())
    })?;
    let required = get_bool_attr(obj, "required", false);
    let methods = get_list_attr(obj, "methods", |item| {
        marshal_credential_method_entry(item)
    })?;
    Ok(CredentialEntry {
        name,
        hosts,
        required,
        methods,
    })
}

unsafe fn marshal_credential_method_entry(
    obj: *mut pyo3_ffi::PyObject,
) -> Result<CredentialMethodEntry, String> {
    let name = get_string_attr(obj, "name").unwrap_or_default();
    let description = get_string_attr(obj, "description").unwrap_or_default();
    let method = {
        let cn = CString::new("method").unwrap();
        let m = pyo3_ffi::PyObject_GetAttrString(obj, cn.as_ptr());
        if m.is_null() {
            pyo3_ffi::PyErr_Clear();
            return Err(format!("credential-method-entry {name:?}: missing .method"));
        }
        let r = marshal_credential_method(m);
        pyo3_ffi::Py_DecRef(m);
        r?
    };
    Ok(CredentialMethodEntry {
        name,
        description,
        method,
    })
}

unsafe fn marshal_credential_method(obj: *mut pyo3_ffi::PyObject) -> Result<CredentialMethod, String> {
    let kind = get_string_attr(obj, "kind").unwrap_or_default();
    match kind.as_str() {
        "basic" => Ok(CredentialMethod::Basic),
        "raw" => Ok(CredentialMethod::Raw),
        "oauth2" => {
            let flows = get_list_attr(obj, "flows", |item| {
                let s = pyunicode_to_string(item).unwrap_or_default();
                match s.as_str() {
                    "authorization-code" => Ok(Oauth2Flow::AuthorizationCode),
                    "authorization-code-pkce" => Ok(Oauth2Flow::AuthorizationCodePkce),
                    "device-code" => Ok(Oauth2Flow::DeviceCode),
                    other => Err(format!("unknown OAuth2 flow {other:?}")),
                }
            })?;
            let scopes = get_list_attr(obj, "scopes", |item| {
                Ok(pyunicode_to_string(item).unwrap_or_default())
            })?;
            Ok(CredentialMethod::Oauth2(Oauth2Method {
                flows,
                scopes,
                authorization_url: get_string_attr(obj, "authorization_url").unwrap_or_default(),
                token_url: get_string_attr(obj, "token_url").unwrap_or_default(),
                device_auth_url: get_string_attr(obj, "device_auth_url").unwrap_or_default(),
            }))
        }
        "apikey" => {
            let loc_name = CString::new("location").unwrap();
            let loc = pyo3_ffi::PyObject_GetAttrString(obj, loc_name.as_ptr());
            let location = if loc.is_null() {
                pyo3_ffi::PyErr_Clear();
                None
            } else if is_none(loc) {
                pyo3_ffi::Py_DecRef(loc);
                None
            } else {
                let kind_str = get_string_attr(loc, "kind").unwrap_or_default();
                let kind_enum = match kind_str.as_str() {
                    "header" => ApikeyLocationKind::Header,
                    "auth-scheme" => ApikeyLocationKind::AuthScheme,
                    "query-param" => ApikeyLocationKind::QueryParam,
                    other => {
                        pyo3_ffi::Py_DecRef(loc);
                        return Err(format!("unknown apikey location kind {other:?}"));
                    }
                };
                let name = get_optional_string_attr(loc, "name");
                let scheme = get_optional_string_attr(loc, "scheme");
                pyo3_ffi::Py_DecRef(loc);
                Some(ApikeyLocation {
                    kind: kind_enum,
                    name,
                    scheme,
                })
            };
            Ok(CredentialMethod::Apikey(ApikeyMethod { location }))
        }
        "signing-key" => {
            let alg = get_string_attr(obj, "algorithm").unwrap_or_default();
            let algorithm = match alg.as_str() {
                "hmac-sha256" => SigningAlgorithm::HmacSha256,
                "hmac-sha512" => SigningAlgorithm::HmacSha512,
                other => return Err(format!("unknown signing algorithm {other:?}")),
            };
            Ok(CredentialMethod::SigningKey(SigningKeyMethod { algorithm }))
        }
        other => Err(format!(
            "unsupported credential-method kind {other:?} — bootstrap should map to basic/raw/oauth2/apikey/signing-key"
        )),
    }
}

export!(Component);
