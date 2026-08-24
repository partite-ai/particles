// Package trace implements integration with Go's trace package.
package trace

import (
	"context"
	"runtime/trace"

	"github.com/partite-ai/wacogo"
)

type ctxKey struct{}
type regionHolder struct {
	region *trace.Region
}

func TracedCall(ctx context.Context, name string, fn *wacogo.ExportedFunc, args ...wacogo.Val) ([]wacogo.Val, error) {
	taskCtx, task := trace.NewTask(ctx, "wasmcall-"+name)
	defer task.End()

	region := trace.StartRegion(taskCtx, "wasm")
	holder := &regionHolder{region: region}
	taskCtx = context.WithValue(taskCtx, ctxKey{}, holder)
	defer func() {
		holder.region.End()
	}()

	return fn.Call(taskCtx, args...)
}

func StartHostCall(ctx context.Context, name string) {
	rhAny := ctx.Value(ctxKey{})
	if rhAny == nil {
		return
	}
	regionHolder := rhAny.(*regionHolder)

	regionHolder.region.End()
	region := trace.StartRegion(ctx, "hostcall-"+name)
	regionHolder.region = region
}

func EndHostCall(ctx context.Context) {
	rhAny := ctx.Value(ctxKey{})
	if rhAny == nil {
		return
	}
	regionHolder := rhAny.(*regionHolder)

	regionHolder.region.End()
	region := trace.StartRegion(ctx, "wasm")
	regionHolder.region = region
}
