package bootstrap

import (
	"context"
	"sync"
)

// backgroundGroup joins background database/storage users before dependencies close.
type backgroundGroup struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newBackgroundGroup(parent context.Context) (context.Context, *backgroundGroup) {
	ctx, cancel := context.WithCancel(parent)
	return ctx, &backgroundGroup{cancel: cancel}
}
func (g *backgroundGroup) Go(fn func()) { g.wg.Add(1); go func() { defer g.wg.Done(); fn() }() }
func (g *backgroundGroup) Close()       { g.cancel(); g.wg.Wait() }
