package wacogo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	wc "github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/wasi"
)

// pipInstallerInterface is the canonical id of the exported instance
// the pip-resolve component publishes — see
// components/pip-resolve/wit/world.wit.
const pipInstallerInterface = "particle:build/pip-installer@0.1.0"

// PipResolvedWheel is one entry in a PipResolveResult.
type PipResolvedWheel struct {
	// Name is the PEP 503-normalized distribution name (lowercased,
	// `_`/`.`/`-` collapsed). Suitable for filename / lockfile keys.
	Name string
	// Version is the PEP 440 version string the resolver picked.
	Version string
	// Sha256 is "sha256:<hex>", matching PyPI's published digest.
	Sha256 string
	// Filename is the original wheel filename
	// (`<dist>-<ver>-<pytag>-<abi>-<platform>.whl`). Always
	// `*-none-any.whl` for our pipeline — the component rejects
	// platform-tagged wheels at resolve.
	Filename string
	// WheelBytes is the verified wheel as raw bytes. Caller is
	// expected to either unzip into the artifact's
	// `_deps/site-packages` or store the bytes verbatim.
	WheelBytes []byte
}

// PipResolveResult is what Components.PipResolveAndFetch returns on
// success. Stderr captures whatever the component wrote to
// wasi:cli/stderr — typically empty on a clean run.
type PipResolveResult struct {
	Wheels []PipResolvedWheel
	Stderr []byte
}

// PipResolveAndFetch resolves the PEP 508 `reqs` against PyPI, fetches
// the transitive closure of pure-Python wheels, and returns each one
// with verified sha256 and the raw bytes.
//
// `pythonVersion` is informational for v1 (the component doesn't yet
// evaluate environment markers); reserved for future use.
//
// Failure modes are surfaced as typed errors via decodePipError:
// network failures talking to PyPI, malformed PyPI responses, missing
// versions / conflicting constraints, packages whose only published
// wheels carry a compiled-ABI or platform tag, and sha256 mismatches.
func (c *Components) PipResolveAndFetch(ctx context.Context, reqs []string, pythonVersion string) (*PipResolveResult, error) {
	pipResolve, err := c.loadEmbedded(ctx, c.pipResolve)
	if err != nil {
		return nil, err
	}

	stderrBuf := &bytes.Buffer{}
	w, err := wasi.NewWorld(ctx, c.engine, &wasi.Config{
		Args: []string{"pip-resolve"},
		// No Preopens: the component reads no host filesystem; it
		// talks to PyPI over wasi:http (just like deno-npm).
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: stderrBuf,
	})
	if err != nil {
		return nil, fmt.Errorf("build wasi world: %w", err)
	}
	defer w.Close(ctx)

	inst, err := pipResolve.Instantiate(ctx, w.Imports()...)
	if err != nil {
		return nil, withStderr(err, stderrBuf, "instantiate pip-resolve")
	}
	defer inst.Close(ctx)

	iface := inst.ExportedInstance(pipInstallerInterface)
	if iface == nil {
		return nil, fmt.Errorf("pip-resolve component does not export instance %q", pipInstallerInterface)
	}
	fn := iface.ExportedFunc("resolve-and-fetch")
	if fn == nil {
		return nil, fmt.Errorf("pip-resolve.%s does not export resolve-and-fetch()", pipInstallerInterface)
	}

	// Build the list<pip-request>. Dedup first — the same req
	// appearing twice would just waste a PyPI fetch and let the
	// resolver re-process; cheaper to collapse here.
	deduped := dedupReqs(reqs)
	args := make([]wc.Val, len(deduped))
	for i, r := range deduped {
		args[i] = wc.NewValRecord(field("requirement", wc.ValString(r)))
	}
	reqList := wc.NewValListOf[wc.Val](args...)

	results, err := fn.Call(ctx, reqList, wc.ValString(pythonVersion))
	if err != nil {
		return &PipResolveResult{Stderr: stderrBuf.Bytes()},
			withStderr(err, stderrBuf, "call pip-installer.resolve-and-fetch")
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("pip-resolve.resolve-and-fetch returned %d results, want 1", len(results))
	}
	res, ok := results[0].(*wc.ValResult)
	if !ok {
		return nil, fmt.Errorf("pip-resolve result is %T, want *wacogo.ValResult", results[0])
	}
	if !res.IsOk() {
		return &PipResolveResult{Stderr: stderrBuf.Bytes()}, decodePipError(res.Err())
	}

	list, ok := res.Ok().(*wc.ValList)
	if !ok {
		return nil, fmt.Errorf("pip-resolve ok payload is %T, want *wacogo.ValList", res.Ok())
	}
	wheels, err := assemblePipWheels(list)
	if err != nil {
		return nil, err
	}
	return &PipResolveResult{Wheels: wheels, Stderr: stderrBuf.Bytes()}, nil
}

func decodePipError(v wc.Val) error {
	variant, ok := v.(*wc.ValVariant)
	if !ok {
		return fmt.Errorf("pip-resolve error payload is %T, want *wacogo.ValVariant", v)
	}
	// Order matches the WIT variant declaration in
	// components/pip-resolve/wit/world.wit.
	cases := []string{
		"network-error",
		"invalid-pypi-response",
		"resolution-error",
		"no-pure-python-wheel",
		"integrity-mismatch",
	}
	d := int(variant.Discriminant())
	name := "unknown"
	if d >= 0 && d < len(cases) {
		name = cases[d]
	}
	msg := ""
	if s, ok := variant.Val().(wc.ValString); ok {
		msg = string(s)
	}
	return fmt.Errorf("pip-resolve: %s: %s", name, msg)
}

func assemblePipWheels(list *wc.ValList) ([]PipResolvedWheel, error) {
	out := make([]PipResolvedWheel, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		rec, ok := list.Get(i).(*wc.ValRecord)
		if !ok {
			return nil, fmt.Errorf("resolved-wheel[%d] is %T, want *wacogo.ValRecord", i, list.Get(i))
		}
		name := stringField(rec, "name")
		version := stringField(rec, "version")
		sha := stringField(rec, "sha256")
		filename := stringField(rec, "filename")
		wheelBytes, err := bytesField(rec, "wheel-bytes")
		if err != nil {
			return nil, fmt.Errorf("resolved-wheel[%d].wheel-bytes: %w", i, err)
		}
		out = append(out, PipResolvedWheel{
			Name:       name,
			Version:    version,
			Sha256:     sha,
			Filename:   filename,
			WheelBytes: wheelBytes,
		})
	}
	return out, nil
}

func dedupReqs(reqs []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range reqs {
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}
