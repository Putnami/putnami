package runnersource

import "os"

// Windows has no filesystem FIFOs; root containment and fstat still apply.
const sourceReadFlags = os.O_RDONLY
