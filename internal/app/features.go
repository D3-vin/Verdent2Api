// features.go — bridge-local toggles. The Z.AI per-model feature machinery
// is gone with the Z.AI upstream; only the conversation-history switch
// remains (env PERSIST_HISTORY=true).
package app

import (
	"os"
	"sync/atomic"
)

// persistHistory gates conversation history persistence in the
// ConversationStore.
var persistHistory atomic.Bool

func init() {
	if os.Getenv("PERSIST_HISTORY") == "true" {
		persistHistory.Store(true)
	}
}

func persistHistoryEnabled() bool { return persistHistory.Load() }
