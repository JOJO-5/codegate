package agent

import (
	"context"
	"os"
	"time"

	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/updatefile"
)

func (a *Agent) writeManagedHealth(authenticated bool) {
	marker := os.Getenv("CODEGATE_AGENT_HEALTH_FILE")
	// Keep the legacy one-shot marker so an older launcher can upgrade its child.
	if authenticated {
		if legacy := os.Getenv("CODEGATE_AGENT_READY_FILE"); legacy != "" {
			_ = os.WriteFile(legacy, []byte(Version), 0600)
		}
	}
	if marker != "" {
		if err := updatefile.WriteJSON(marker, protocol.ManagedHealth{Version: Version, Authenticated: authenticated, UpdatedAt: time.Now().UnixMilli()}); err != nil {
			a.log.Warn("写入守护健康状态失败", "err", err)
		}
	}
}

// A disconnected Server is not a hung Agent. Report progress while backing off,
// but never label a reconnecting child authenticated for initial acceptance.
func (a *Agent) waitReconnect(ctx context.Context, delay time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(delay)
		defer timer.Stop()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				return
			case <-ticker.C:
				a.writeManagedHealth(false)
			}
		}
	}()
	return done
}
