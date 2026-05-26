package runtime

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/fs"
)

// embeddedPythonStdlibPath is the go:embed-rooted path to the zip
// archive of CPython 3.14's `install/lib/python3.14/` directory.
// Built by the `python-stdlib-zip` Makefile target; the host mounts
// the zip's contents as a wasi preopen for the python runtime so
// the runtime sees /usr/local/lib/python3.14/... at startup.
const embeddedPythonStdlibPath = "embed/python3.14-stdlib.zip"

func pythonStdlibFS() (fs.FS, error) {
	b, err := embeddedRuntime.ReadFile(embeddedPythonStdlibPath)
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", embeddedPythonStdlibPath, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("open embedded python-stdlib zip: %w", err)
	}
	return zr, nil
}

// embeddedPythonBootstrapPath is the go:embed-rooted path to the
// zip of the runtime-side python sources (bootstrap.py + the
// particle/ helper package). The host mounts this zip at /runtime
// in the python runtime's wasi preopen; the runtime's
// ensure_python_initialized inserts /runtime on sys.path before
// `import bootstrap`.
const embeddedPythonBootstrapPath = "embed/python-runtime-bootstrap.zip"

// pythonBootstrapFS opens the runtime-side bootstrap zip as an
// fs.FS rooted at the bootstrap dir's top (bootstrap.py +
// particle/).
func pythonBootstrapFS() (fs.FS, error) {
	b, err := embeddedRuntime.ReadFile(embeddedPythonBootstrapPath)
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", embeddedPythonBootstrapPath, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("open embedded python-runtime-bootstrap zip: %w", err)
	}
	return zr, nil
}
