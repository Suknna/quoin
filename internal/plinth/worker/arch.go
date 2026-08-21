package worker

import "runtime"

// archToken is the compiled GOARCH (used to select the frozen
// architecture-specific read-only runtime paths).
var archToken = runtime.GOARCH
