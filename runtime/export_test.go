package runtime

import wc "github.com/partite-ai/wacogo"

// OwnComponentOf is the component p loaded for itself, for tests.
func OwnComponentOf(p *Particle) *wc.Component { return p.ownComp }
